package main

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

func main() {
	fmt.Println("not implement")
}

type externalInterface struct {
	Iface     *net.Interface
	IfaceAddr net.IP
	ExtAddr   net.IP
}

func lookupExtIface() (*externalInterface, error) {
	var iface *net.Interface
	var ifaceAddr net.IP
	var err error

	logrus.Info("Determining IP address of default interface")
	if iface, err = getDefaultGatewayIface(); err != nil {
		return nil, fmt.Errorf("failed to get default interface: %s", err)
	}

	if ifaceAddr == nil {
		ifaceAddr, err = getIfaceIP4Addr(iface)
		if err != nil {
			return nil, fmt.Errorf("failed to find IPv4 address for interface %s", iface.Name)
		}
	}

	logrus.Infof("Using interface with name %s and address %s", iface.Name, ifaceAddr)

	if iface.MTU == 0 {
		return nil, fmt.Errorf("failed to determine MTU for %s interface", ifaceAddr)
	}

	var extAddr net.IP
	if extAddr == nil {
		logrus.Infof("Defaulting external address to interface address (%s)", ifaceAddr)
		extAddr = ifaceAddr
	}

	return &externalInterface{
		Iface:     iface,
		IfaceAddr: ifaceAddr,
		ExtAddr:   extAddr,
	}, nil
}

func getDefaultGatewayIface() (*net.Interface, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return nil, err
	}

	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" {
			if route.LinkIndex <= 0 {
				return nil, errors.New("Found default route but could not determine interface")
			}
			return net.InterfaceByIndex(route.LinkIndex)
		}
	}

	return nil, errors.New("Unable to find default route")
}

func getIfaceAddrs(iface *net.Interface) ([]netlink.Addr, error) {
	link := &netlink.Device{
		netlink.LinkAttrs{
			Index: iface.Index,
		},
	}

	return netlink.AddrList(link, syscall.AF_INET)
}

func getIfaceIP4Addr(iface *net.Interface) (net.IP, error) {
	addrs, err := getIfaceAddrs(iface)
	if err != nil {
		return nil, err
	}

	// prefer non link-local addr
	var ll net.IP

	for _, addr := range addrs {
		if addr.IP.To4() == nil {
			continue
		}

		if addr.IP.IsGlobalUnicast() {
			return addr.IP, nil
		}

		if addr.IP.IsLinkLocalUnicast() {
			ll = addr.IP
		}
	}

	if ll != nil {
		// didn't find global but found link-local. it'll do.
		return ll, nil
	}

	return nil, errors.New("No IPv4 address found for given interface")
}