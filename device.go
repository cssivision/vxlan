package main

import (
	"fmt"
	"net"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

type vxlanDeviceAttrs struct {
	vni       uint32
	name      string
	vtepIndex int
	vtepAddr  net.IP
	vtepPort  int
	gbp       bool
}

type vxlanDevice struct {
	link          *netlink.Vxlan
	directRouting bool
}

func newVxlanDevice(devAttrs *vxlanDeviceAttrs) (*vxlanDevice, error) {
	link := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: devAttrs.name,
		},
		VxlanId:      int(devAttrs.vni),
		VtepDevIndex: devAttrs.vtepIndex,
		SrcAddr:      devAttrs.vtepAddr,
		Port:         devAttrs.vtepPort,
		Learning:     false,
		GBP:          devAttrs.gbp,
	}

	link, err := ensureLink(link)
	if err != nil {
		return nil, err
	}
	return &vxlanDevice{
		link: link,
	}, nil
}

func ensureLink(vxlan *netlink.Vxlan) (*netlink.Vxlan, error) {
	err := netlink.LinkAdd(vxlan)
	if err == syscall.EEXIST {
		// it's ok if the device already exists as long as config is similar
		logrus.Infof("VXLAN device already exists")
		existing, err := netlink.LinkByName(vxlan.Name)
		if err != nil {
			return nil, err
		}

		incompat := vxlanLinksIncompat(vxlan, existing)
		if incompat == "" {
			logrus.Infof("Returning existing device")
			return existing.(*netlink.Vxlan), nil
		}

		// delete existing
		logrus.Warningf("%q already exists with incompatable configuration: %v; recreating device", vxlan.Name, incompat)
		if err = netlink.LinkDel(existing); err != nil {
			return nil, fmt.Errorf("failed to delete interface: %v", err)
		}

		// create new
		if err = netlink.LinkAdd(vxlan); err != nil {
			return nil, fmt.Errorf("failed to create vxlan interface: %v", err)
		}
	} else if err != nil {
		return nil, err
	}

	ifindex := vxlan.Index
	link, err := netlink.LinkByIndex(vxlan.Index)
	if err != nil {
		return nil, fmt.Errorf("can't locate created vxlan device with index %v", ifindex)
	}

	var ok bool
	if vxlan, ok = link.(*netlink.Vxlan); !ok {
		return nil, fmt.Errorf("created vxlan device with index %v is not vxlan", ifindex)
	}

	return vxlan, nil
}

func vxlanLinksIncompat(l1, l2 netlink.Link) string {
	if l1.Type() != l2.Type() {
		return fmt.Sprintf("link type: %v vs %v", l1.Type(), l2.Type())
	}

	v1 := l1.(*netlink.Vxlan)
	v2 := l2.(*netlink.Vxlan)

	if v1.VxlanId != v2.VxlanId {
		return fmt.Sprintf("vni: %v vs %v", v1.VxlanId, v2.VxlanId)
	}

	if v1.VtepDevIndex > 0 && v2.VtepDevIndex > 0 && v1.VtepDevIndex != v2.VtepDevIndex {
		return fmt.Sprintf("vtep (external) interface: %v vs %v", v1.VtepDevIndex, v2.VtepDevIndex)
	}

	if len(v1.SrcAddr) > 0 && len(v2.SrcAddr) > 0 && !v1.SrcAddr.Equal(v2.SrcAddr) {
		return fmt.Sprintf("vtep (external) IP: %v vs %v", v1.SrcAddr, v2.SrcAddr)
	}

	if len(v1.Group) > 0 && len(v2.Group) > 0 && !v1.Group.Equal(v2.Group) {
		return fmt.Sprintf("group address: %v vs %v", v1.Group, v2.Group)
	}

	if v1.L2miss != v2.L2miss {
		return fmt.Sprintf("l2miss: %v vs %v", v1.L2miss, v2.L2miss)
	}

	if v1.Port > 0 && v2.Port > 0 && v1.Port != v2.Port {
		return fmt.Sprintf("port: %v vs %v", v1.Port, v2.Port)
	}

	if v1.GBP != v2.GBP {
		return fmt.Sprintf("gbp: %v vs %v", v1.GBP, v2.GBP)
	}

	return ""
}

func (dev *vxlanDevice) configure(ipn string) error {
	if err := ensureV4AddressOnLink(ipn, dev.link); err != nil {
		return fmt.Errorf("failed to ensure address of interface %s: %s", dev.link.Attrs().Name, err)
	}

	if err := netlink.LinkSetUp(dev.link); err != nil {
		return fmt.Errorf("failed to set interface %s to UP state: %s", dev.link.Attrs().Name, err)
	}

	return nil
}

func (dev *vxlanDevice) handleSubnetEvents(batch []Event) {
	for _, event := range batch {
		sn := event.Subnet
		attrs := event.Attrs

		// This route is used when traffic should be vxlan encapsulated
		vxlanRoute := netlink.Route{
			LinkIndex: dev.link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       sn.ToIPNet(),
			Gw:        sn.IP.ToIP(),
		}
		vxlanRoute.SetFlag(syscall.RTNH_F_ONLINK)

		if event.Type == eventAdd {
			logrus.Infof("adding subnet: %s PublicIP: %s VtepMAC: %s", sn.StringSep(".", "/"), attrs.PublicIP.ToIP(), net.HardwareAddr(attrs.HardwareAddr))
			if err := dev.AddARP(neighbor{IP: sn.IP.ToIP(), MAC: net.HardwareAddr(attrs.HardwareAddr)}); err != nil {
				logrus.Error("AddARP failed: ", err)
				continue
			}

			if err := dev.AddFDB(neighbor{IP: attrs.PublicIP.ToIP(), MAC: net.HardwareAddr(attrs.HardwareAddr)}); err != nil {
				logrus.Error("AddFDB failed: ", err)

				// Try to clean up the ARP entry then continue
				if err := dev.DelARP(neighbor{IP: sn.IP.ToIP(), MAC: net.HardwareAddr(attrs.HardwareAddr)}); err != nil {
					logrus.Error("DelARP failed: ", err)
				}

				continue
			}

			// Set the route - the kernel would ARP for the Gw IP address if it hadn't already been set above so make sure
			// this is done last.
			if err := netlink.RouteReplace(&vxlanRoute); err != nil {
				logrus.Errorf("failed to add vxlanRoute (%s -> %s): %v", vxlanRoute.Dst, vxlanRoute.Gw, err)

				// Try to clean up both the ARP and FDB entries then continue
				if err := dev.DelARP(neighbor{IP: sn.IP.ToIP(), MAC: net.HardwareAddr(attrs.HardwareAddr)}); err != nil {
					logrus.Error("DelARP failed: ", err)
				}

				if err := dev.DelFDB(neighbor{IP: attrs.PublicIP.ToIP(), MAC: net.HardwareAddr(attrs.HardwareAddr)}); err != nil {
					logrus.Error("DelFDB failed: ", err)
				}

				continue
			}
		} else {
			logrus.Infof("invalid event type: %v\n", event.Type)
		}
	}
}

type neighbor struct {
	MAC net.HardwareAddr
	IP  net.IP
}

func (dev *vxlanDevice) AddFDB(n neighbor) error {
	logrus.Infof("calling AddFDB: %v, %v", n.IP, n.MAC)
	return netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    dev.link.Index,
		State:        netlink.NUD_PERMANENT,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_SELF,
		IP:           n.IP,
		HardwareAddr: n.MAC,
	})
}

func (dev *vxlanDevice) DelFDB(n neighbor) error {
	logrus.Infof("calling DelFDB: %v, %v", n.IP, n.MAC)
	return netlink.NeighDel(&netlink.Neigh{
		LinkIndex:    dev.link.Index,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_SELF,
		IP:           n.IP,
		HardwareAddr: n.MAC,
	})
}

func (dev *vxlanDevice) AddARP(n neighbor) error {
	logrus.Infof("calling AddARP: %v, %v", n.IP, n.MAC)
	return netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    dev.link.Index,
		State:        netlink.NUD_PERMANENT,
		Type:         syscall.RTN_UNICAST,
		IP:           n.IP,
		HardwareAddr: n.MAC,
	})
}

func (dev *vxlanDevice) DelARP(n neighbor) error {
	logrus.Infof("calling DelARP: %v, %v", n.IP, n.MAC)
	return netlink.NeighDel(&netlink.Neigh{
		LinkIndex:    dev.link.Index,
		State:        netlink.NUD_PERMANENT,
		Type:         syscall.RTN_UNICAST,
		IP:           n.IP,
		HardwareAddr: n.MAC,
	})
}

// ensureV4AddressOnLink ensures that there is only one v4 Addr on `link` and it equals `ipn`.
// If there exist multiple addresses on link, it returns an error message to tell callers to remove additional address.
func ensureV4AddressOnLink(ipn string, link netlink.Link) error {
	addr, err := netlink.ParseAddr(ipn)
	if err != nil {
		return fmt.Errorf("parse address error: %v", err)
	}

	existingAddrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}

	// this will never happen. This situation can only be caused by a user, so get them to sort it out.
	if len(existingAddrs) > 1 {
		return fmt.Errorf("link has incompatible addresses. Remove additional addresses and try again. %#v", link)
	}

	// If the device has an incompatible address then delete it. This can happen if the lease changes for example.
	if len(existingAddrs) == 1 && !existingAddrs[0].Equal(*addr) {
		if err := netlink.AddrDel(link, &existingAddrs[0]); err != nil {
			return fmt.Errorf("failed to remove IP address %s from %s: %s", ipn, link.Attrs().Name, err)
		}
		existingAddrs = []netlink.Addr{}
	}

	// Actually add the desired address to the interface if needed.
	if len(existingAddrs) == 0 {
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("failed to add IP address %s to %s: %s", ipn, link.Attrs().Name, err)
		}
	}

	return nil
}
