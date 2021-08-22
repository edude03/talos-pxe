package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/milosgajdos/tenus"
	web "github.com/poseidon/matchbox/matchbox/http"
	"github.com/poseidon/matchbox/matchbox/server"
	"github.com/poseidon/matchbox/matchbox/storage"
)

var log = logrus.New()

const (
	serverRoot = "."
	portDHCP = 67
	portTFTP = 69
	portHTTP = 8080
	portPXE  = 4011
)

// Architecture describes a kind of CPU architecture.
type Architecture int

// Architecture types that Pixiecore knows how to boot.
//
// These architectures are self-reported by the booting machine. The
// machine may support additional execution modes. For example, legacy
// PC BIOS reports itself as an ArchIA32, but may also support ArchX64
// execution.
const (
	// ArchIA32 is a 32-bit x86 machine. It _may_ also support X64
	// execution, but Pixiecore has no way of knowing.
	ArchIA32 Architecture = iota
	// ArchX64 is a 64-bit x86 machine (aka amd64 aka X64).
	ArchX64
)

func (a Architecture) String() string {
	switch a {
	case ArchIA32:
		return "IA32"
	case ArchX64:
		return "X64"
	default:
		return "Unknown architecture"
	}
}

// A Machine describes a machine that is attempting to boot.
type Machine struct {
	MAC  net.HardwareAddr
	Arch Architecture
}

// Firmware describes a kind of firmware attempting to boot.
//
// This should only be used for selecting the right bootloader within
// Pixiecore, kernel selection should key off the more generic
// Architecture.
type Firmware int

// The bootloaders that Pixiecore knows how to handle.
const (
	FirmwareX86PC         Firmware = iota // "Classic" x86 BIOS with PXE/UNDI support
	FirmwareEFI32                         // 32-bit x86 processor running EFI
	FirmwareEFI64                         // 64-bit x86 processor running EFI
	FirmwareEFIBC                         // 64-bit x86 processor running EFI
	FirmwareX86Ipxe                       // "Classic" x86 BIOS running iPXE (no UNDI support)
	FirmwarePixiecoreIpxe                 // Pixiecore's iPXE, which has replaced the underlying firmware
)

type DHCPRecord struct {
	IP net.IP
	expires time.Time
}

// A Server boots machines using a Booter.
type Server struct {
	ServerRoot string

	IP net.IP

	Net *net.IPNet

	Intf string

	ProxyDHCP bool

	// Log receives logs on Pixiecore's operation. If nil, logging
	// is suppressed.
	Log func(subsystem, msg string)
	// Debug receives extensive logging on Pixiecore's internals. Very
	// useful for debugging, but very verbose.
	Debug func(subsystem, msg string)

	DHCPLock sync.Mutex
	DHCPRecords map[string]*DHCPRecord

	IPLock sync.Mutex
	IPRecords map[string]bool

	// These ports can technically be set for testing, but the
	// protocols burned in firmware on the client side hardcode these,
	// so if you change them in production, nothing will work.
	DHCPPort int
	TFTPPort int
	PXEPort  int
	HTTPPort int

	errs chan error

	eventsMu sync.Mutex
	events   map[string][]machineEvent
}

func (s *Server) Ipxe(classId, classInfo string) ([]byte, error) {
	var resultBuffer bytes.Buffer

	if classId == "PXEClient:Arch:00000:UNDI:002001" && classInfo == "[iPXE]" {
		ipxeMenuTemplate.Execute(&resultBuffer, s)
		return resultBuffer.Bytes(), nil
	}

	return nil, fmt.Errorf("Unknown class %s:%s", classId, classInfo)
}

// Serve listens for machines attempting to boot, and uses Booter to
// help them.
func (s *Server) Serve() error {
	if s.DHCPPort == 0 {
		s.DHCPPort = portDHCP
	}
	if s.TFTPPort == 0 {
		s.TFTPPort = portTFTP
	}
	if s.PXEPort == 0 {
		s.PXEPort = portPXE
	}
	if s.HTTPPort == 0 {
		s.HTTPPort = portHTTP
	}

	if s.ServerRoot == "" {
		s.ServerRoot = serverRoot
	}

	tftp, err := net.ListenPacket("udp", fmt.Sprintf("%s:%d", s.IP, s.TFTPPort))
	if err != nil {
		return err
	}
	pxe, err := net.ListenPacket("udp4", fmt.Sprintf("%s:%d", s.IP, s.PXEPort))
	if err != nil {
		tftp.Close()
		return err
	}
	http, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.IP, s.HTTPPort))
	if err != nil {
		tftp.Close()
		pxe.Close()
		return err
	}

	s.events = make(map[string][]machineEvent)
	// 5 buffer slots, one for each goroutine, plus one for
	// Shutdown(). We only ever pull the first error out, but shutdown
	// will likely generate some spurious errors from the other
	// goroutines, and we want them to be able to dump them without
	// blocking.
	s.errs = make(chan error, 6)

	s.debug("Init", "Starting servers")

	go func() { s.errs <- s.servePXE(pxe) }()
	go func() { s.errs <- s.serveTFTP(tftp) }()
	go func() { s.errs <- s.startMatchbox(http) }()
	go func() { s.errs <- s.startDhcp() }()

	// Wait for either a fatal error, or Shutdown().
	err = <-s.errs
	http.Close()
	tftp.Close()
	pxe.Close()
	return err
}

func (s *Server) startMatchbox(l net.Listener) error {
	store := storage.NewFileStore(&storage.Config{
		Root: s.ServerRoot,
	})

	server := server.NewServer(&server.Config{
		Store: store,
	})

	config := &web.Config{
		Core: server,
		Logger: log,
		AssetsPath: filepath.Join(s.ServerRoot, "assets"),
	}

	httpServer := web.NewServer(config)
	if err := http.Serve(l, s.ipxeWrapperMenuHandler(httpServer.HTTPHandler())); err != nil {
		return fmt.Errorf("Matchbox server shut down: %s", err)
	}

	return nil
}

// Shutdown causes Serve() to exit, cleaning up behind itself.
func (s *Server) Shutdown() {
	select {
	case s.errs <- nil:
	default:
	}
}

var ipxeMenuTemplate = template.Must(template.New("iPXE Menu").Parse(`#!ipxe
isset ${proxydhcp/next-server} || goto start
set next-server ${proxydhcp/next-server}
set filename ${proxydhcp/filename}

:start
menu iPXE boot menu for Talos
item --gap                      Talos Nodes
item --key i init               Bootstrap Node
item --key c controlplane       Master Node
item --key w worker             Worker Node
item --gap                      Other
item --key s shell              iPXE Shell
item --key r reboot             Reboot
item --key e exit               Exit
choose --timeout 0 --default worker selected || goto cancel
set menu-timeout 0
goto ${selected}

:init
chain http://{{ .IP }}:8080/ipxe?uuid=${uuid}&mac=${mac:hexhyp}&domain=${domain}&hostname=${hostname}&serial=${serial}&type=init

:controlplane
chain http://{{ .IP }}:8080/ipxe?uuid=${uuid}&mac=${mac:hexhyp}&domain=${domain}&hostname=${hostname}&serial=${serial}&type=controlplane

:worker
chain http://{{ .IP }}:8080/ipxe?uuid=${uuid}&mac=${mac:hexhyp}&domain=${domain}&hostname=${hostname}&serial=${serial}&type=worker

:reboot
reboot

:shell
shell

:exit
exit
`))

type ProxyServer struct {
	DhcpIp string
	Netmask string
}

type StandaloneServer struct {
	IpMin, IpMax string
}

func getPrivateAddress() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr).IP

	return localAddr, nil
}

func getInterface(addr net.IP) (*net.Interface, net.IPMask, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}

	for _, iface := range ifaces {
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			return nil, nil, err
		}

		for _, ifaceAddr := range ifaceAddrs {
			switch v := ifaceAddr.(type) {
				case *net.IPAddr:
					if v.IP.Equal(addr) {
						return &iface, v.IP.DefaultMask(), nil
					}

				case *net.IPNet:
					if v.IP.Equal(addr) {
						return &iface, v.Mask, nil
					}
			}
		}
	}

	return nil, nil, fmt.Errorf("Could not find interface for address")
}

func getValidInterfaces() ([]net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var validInterfaces []net.Interface

	for _, iface := range ifaces {
		if iface.Flags & net.FlagLoopback != 0 {
			continue
		}

		if iface.Flags & net.FlagUp == 0 {
			continue
		}

		validInterfaces = append(validInterfaces, iface)
	}

	if len(validInterfaces) == 0 {
		return nil, fmt.Errorf("Could not find any non-loopback interfaces that are active")
	}

	return validInterfaces, nil
}

func runDhclient(ctx context.Context, iface *net.Interface) (*dhclient.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	leaseCh := make(chan *dhclient.Lease)
	client := dhclient.Client{
		Iface: iface,
		OnBound: func(lease *dhclient.Lease) {
			leaseCh <- lease
		},
	}

	for _, param := range dhclient.DefaultParamsRequestList {
		client.AddParamRequest(layers.DHCPOpt(param))
	}

	hostname, _ := os.Hostname()
	client.AddOption(layers.DHCPOptHostname, []byte(hostname))

	client.Start()
	defer client.Stop()

	select {
	case lease := <-leaseCh:
		return lease, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("Could not get DHCP")
	}
}

func (s *Server) ipxeWrapperMenuHandler(primaryHandler http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "ipxe" && req.URL.Path != "/ipxe" {
			primaryHandler.ServeHTTP(w, req)
			return
		}

		rr := httptest.NewRecorder()
		primaryHandler.ServeHTTP(rr, req)

		if status := rr.Code; status == http.StatusOK {
			for key, values := range rr.HeaderMap {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}

			w.WriteHeader(rr.Code)

			w.Write(rr.Body.Bytes())
		} else {
			log.Info("Serving menu")

			if err := ipxeMenuTemplate.Execute(w, s); err != nil {
				log.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	}

	return http.HandlerFunc(fn)
}

func main() {
	serverRootFlag := flag.String("root", "", "Server root, where to serve the files from")
	flag.Parse()

	eth0, err := tenus.NewLinkFrom("eth0")
	if err != nil {
		log.Panic(err)
	}

	if err := eth0.SetLinkUp(); err != nil {
		log.Panic(err)
	}

	log.Infof("Brought %s up\n", eth0.NetInterface().Name)

	validInterfaces, err := getValidInterfaces()
	if err != nil {
		log.Panic(err)
	}

	log.Infof("Valid interfaces are:\n")
	for _, iface := range validInterfaces {
		log.Infof(" - %s\n", iface.Name)
	}

	lease, err := runDhclient(context.Background(), eth0.NetInterface())

	server := &Server{
		ServerRoot: *serverRootFlag,
		Intf: eth0.NetInterface().Name,
		IPRecords: make(map[string]bool),
		DHCPRecords: make(map[string]*DHCPRecord),
		Log: func(subsystem, msg string) {
			log.Infof("%s: %s", subsystem, msg)
		},
		Debug: func(subsystem, msg string) {
			log.Infof("%s: %s", subsystem, msg)
		},
	}

	if lease != nil {
		log.Infof("Obtained address %s\n", lease.FixedAddress)

		net := &net.IPNet{
			IP: lease.FixedAddress,
			Mask: lease.Netmask,
		}

		if err := eth0.SetLinkIp(net.IP, net); err != nil {
			log.Panic(err)
		}

		server.IP = lease.FixedAddress
		server.ProxyDHCP = true
	} else {
		netIp, netNet, err := net.ParseCIDR("192.168.123.1/24")

		server.IP = netIp
		server.Net = netNet
		server.ProxyDHCP = false

		fmt.Printf("Setting manual address %s, leasing out subnet %s\n", netIp, netNet)

		if err != nil {
			log.Panic(err)
		}

		if err := eth0.SetLinkIp(netIp, netNet); err != nil && err != syscall.EEXIST {
			log.Panic(err)
		}
	}

	if err := server.Serve(); err != nil {
		log.Panic(err)
	}
}
