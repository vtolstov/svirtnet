package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"crypto/tls"

	"code.google.com/p/go.net/ipv4"
	"code.google.com/p/go.net/ipv6"
	"github.com/alexzorin/libvirt-go"
	"github.com/golang/glog"
)

type IP struct {
	Family  string `xml:"family,attr"`
	Address string `xml:"address,attr"`
	Prefix  string `xml:"prefix,attr,omitempty"`
	Peer    string `xml:"peer,attr,omitempty"`
	Host    string `xml:"host,attr,omitempty"`
	Gateway string `xml:"gateway,attr,omitempty"`
}

type Storage struct {
	Size   string `xml:"size"`
	Target string `xml:"target"`
}

type CloudConfig struct {
	URL string `xml:"url,omitempty"`
}

type Network struct {
	IP []IP `xml:"ip"`
}

type Metadata struct {
	Network     Network     `xml:"network"`
	CloudConfig CloudConfig `xml:"cloud-config"`
}

var httpconn net.Listener

type Server struct {
	// shutdown flag
	shutdown bool

	// domain name
	name string

	// domain metadata
	metadata *Metadata

	// DHCPv4 conn
	ipv4conn *ipv4.RawConn

	// RA conn
	ipv6conn *ipv6.PacketConn

	// Libvirt conn
	libvirt libvirt.VirConnection

	// thread safe
	sync.RWMutex
}

var httpTransport *http.Transport = &http.Transport{
	Dial:            (&net.Dialer{DualStack: true}).Dial,
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}
var httpClient *http.Client = &http.Client{Transport: httpTransport, Timeout: 10 * time.Second}

func cleanExists(name string, ips []IP) []IP {
	ret := make([]IP, len(ips))
	copy(ret[:], ips[:])

	iface, err := net.InterfaceByName("tap" + name)
	if err != nil {
		return ips
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
	loop:
		for i, ip := range ret {
			if ip.Address+"/"+ip.Prefix == addr.String() {
				copy(ret[i:], ret[i+1:])
				ret[len(ret)-1] = IP{}
				ret = ret[:len(ret)-1]
				break loop
			}
		}
	}
	return ret
}

var servers map[string]*Server

func init() {
	servers = make(map[string]*Server, 1024)
}

func (s *Server) Start() error {
	s.Lock()
	var buf string
	var err error
	var domain libvirt.VirDomain

	if s == nil || s.name == "" {
		return errors.New("invalid server config")
	}

	s.libvirt, err = libvirt.NewVirConnectionReadOnly("qemu:///system")
	if err != nil {
		return err
	}

	domain, err = s.libvirt.LookupDomainByName(s.name)
	if err != nil {
		return err
	}

	buf, err = domain.GetMetadata(libvirt.VIR_DOMAIN_METADATA_ELEMENT, "http://simplecloud.ru/", libvirt.VIR_DOMAIN_MEM_LIVE)
	if err != nil {
		return err
	}
	s.metadata = &Metadata{}
	if err = xml.Unmarshal([]byte(buf), s.metadata); err != nil {
		return err
	}

	iface, err := net.InterfaceByName("vlan1001")
	if err != nil {
		return err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}
	var peer string
	var cmd *exec.Cmd
	for _, addr := range addrs {
		a := strings.Split(addr.String(), "/")[0]
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			peer = ip.String()
		}
	}

	metaIP := cleanExists(s.name, s.metadata.Network.IP)

	for _, addr := range metaIP {
		if addr.Family == "ipv4" && addr.Host == "true" {
			// TODO: use netlink
			if addr.Peer != "" {
				cmd = exec.Command("ip", "-4", "a", "add", peer, "peer", addr.Address+"/"+addr.Prefix, "dev", "tap"+s.name)
			} else {
				cmd = exec.Command("ip", "-4", "a", "add", addr.Address+"/"+addr.Prefix, "dev", "tap"+s.name)
			}
			err = cmd.Run()
			if err != nil {
				return fmt.Errorf("Failed to add ip for: %s", addr.Address+"/"+addr.Prefix)
			}
		}
	}

	cmd = exec.Command("sysctl", "-w", "net.ipv4.conf.tap"+s.name+".proxy_arp=1")
	aa, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to enable proxy_arp: %s sysctl -w net.ipv4.conf.tap%s.proxy_arp=1", aa, s.name)
	}

	defer s.Unlock()

	glog.Infof("%s ListenAndServeUDPv4\n", s.name)
	go s.ListenAndServeUDPv4()

	for _, addr := range metaIP {
		if addr.Family == "ipv6" && addr.Host == "true" {
			// TODO: use netlink
			cmd := exec.Command("ip", "-6", "a", "add", addr.Address+"/"+addr.Prefix, "dev", "tap"+s.name)
			err = cmd.Run()
			if err != nil {
				return fmt.Errorf("Failed to add ip for: %s", addr.Address+"/"+addr.Prefix)
			}

			cmd = exec.Command("ip", "-6", "r", "replace", addr.Address+"/"+addr.Prefix, "dev", "tap"+s.name, "proto", "static", "table", "200")
			err = cmd.Run()
			if err != nil {
				return fmt.Errorf("Failed to replace route for: %s", addr.Address+"/"+addr.Prefix)
			}
		}
	}

	glog.Infof("%s ListenAndServeICMPv6\n", s.name)
	go s.ListenAndServeICMPv6()

	select {}
}

func (s *Server) Stop() (err error) {
	s.RLock()
	defer s.RUnlock()
	defer func(s *Server) {
		s.shutdown = true
	}(s)
	if ok, err := s.libvirt.IsAlive(); ok && err == nil {
		err = s.libvirt.UnrefAndCloseConnection()
		if err != nil {
			return err
		}
	}

	if s.ipv4conn != nil {
		err = s.ipv4conn.Close()
		if err != nil {
			return err
		}
	}
	if s.ipv6conn != nil {
		err = s.ipv6conn.Close()
		if err != nil {
			return err
		}
	}

	if s.metadata == nil {
		return nil
	}

	for _, addr := range s.metadata.Network.IP {
		if addr.Family == "ipv6" && addr.Host == "true" {
			/*
				iface, err := net.InterfaceByName("tap" + s.name)
				if err != nil {
					return err
				}
				ip, net, err := net.ParseCIDR(addr.Address + "1/" + addr.Prefix)
				if err != nil {
					return err
				}
				err = netlink.NetworkLinkAddIp(iface, ip, net)
				if err != nil {
					return err
				}
			*/
			// TODO: use netlink
			cmd := exec.Command("ip", "-6", "r", "del", addr.Address+"/"+addr.Prefix, "dev", "tap"+s.name, "proto", "static", "table", "200")
			err = cmd.Run()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func bindToDevice(conn net.PacketConn, device string) error {
	ptrVal := reflect.ValueOf(conn)
	val := reflect.Indirect(ptrVal)
	//next line will get you the net.netFD
	fdmember := val.FieldByName("fd")
	val1 := reflect.Indirect(fdmember)
	netFdPtr := val1.FieldByName("sysfd")
	fd := int(netFdPtr.Int())
	//fd now has the actual fd for the socket
	return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
}

func bindToDevice2(conn *net.TCPListener, device string) error {
	ptrVal := reflect.ValueOf(conn)
	val := reflect.Indirect(ptrVal)
	//next line will get you the net.netFD
	fdmember := val.FieldByName("fd")
	val1 := reflect.Indirect(fdmember)
	netFdPtr := val1.FieldByName("sysfd")
	fd := int(netFdPtr.Int())
	//fd now has the actual fd for the socket
	return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
}
