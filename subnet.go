package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/client"
)

var (
	subnetRegex = regexp.MustCompile(`(\d+\.\d+.\d+.\d+)-(\d+)`)
)

type IP4 uint

// similar to net.IPNet but has uint based representation
type IP4Net struct {
	IP        IP4
	PrefixLen uint
}

func (ip IP4) Octets() (a, b, c, d byte) {
	a, b, c, d = byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip)
	return
}

func (ip IP4) ToIP() net.IP {
	return net.IPv4(ip.Octets())
}

func (ip IP4) StringSep(sep string) string {
	a, b, c, d := ip.Octets()
	return fmt.Sprintf("%d%s%d%s%d%s%d", a, sep, b, sep, c, sep, d)
}

func FromBytes(ip []byte) IP4 {
	return IP4(uint32(ip[3]) |
		(uint32(ip[2]) << 8) |
		(uint32(ip[1]) << 16) |
		(uint32(ip[0]) << 24))
}

func FromIP(ip net.IP) IP4 {
	return FromBytes(ip.To4())
}

func (n IP4Net) StringSep(octetSep, prefixSep string) string {
	return fmt.Sprintf("%s%s%d", n.IP.StringSep(octetSep), prefixSep, n.PrefixLen)
}

func MakeSubnetKey(sn IP4Net) string {
	return sn.StringSep(".", "-")
}

type Attrs struct {
	PublicIP     IP4
	Subnet       IP4Net
	HardwareAddr net.HardwareAddr
}

type manager struct {
	cli    client.KeysAPI
	Prefix string
}

func newManager() manager {
	etcdCli, err := newEtcdClient()
	if err != nil {
		panic(fmt.Sprintf("new etcd client err: %v", err))
	}

	return manager{
		cli:    etcdCli,
		Prefix: "/vxlan",
	}
}

func (m *manager) getSubnets(ctx context.Context) ([]*Attrs, error) {
	key := path.Join(m.Prefix, "subnets")
	resp, err := m.cli.Get(ctx, key, &client.GetOptions{Recursive: true, Quorum: true})
	if err != nil {
		if etcdErr, ok := err.(client.Error); ok && etcdErr.Code == client.ErrorCodeKeyNotFound {
			// key not found: treat it as empty set
			return []*Attrs{}, nil
		}
		return nil, err
	}

	snAttrs := []*Attrs{}

	for _, node := range resp.Node.Nodes {
		l, err := nodeToLease(node)
		if err != nil {
			logrus.Warningf("Ignoring bad subnet node: %v", err)
			continue
		}

		snAttrs = append(snAttrs, l)
	}

	return snAttrs, nil
}

func ParseSubnetKey(s string) *IP4Net {
	if parts := subnetRegex.FindStringSubmatch(s); len(parts) == 3 {
		snIp := net.ParseIP(parts[1]).To4()
		prefixLen, err := strconv.ParseUint(parts[2], 10, 5)
		if snIp != nil && err == nil {
			return &IP4Net{IP: FromIP(snIp), PrefixLen: uint(prefixLen)}
		}
	}

	return nil
}

func nodeToLease(node *client.Node) (*Attrs, error) {
	sn := ParseSubnetKey(node.Key)
	if sn == nil {
		return nil, fmt.Errorf("failed to parse subnet key %s", node.Key)
	}

	attrs := &Attrs{}
	if err := json.Unmarshal([]byte(node.Value), attrs); err != nil {
		return nil, err
	}

	return attrs, nil
}

func (m *manager) createSubnet(ctx context.Context, sn IP4Net, attrs Attrs) error {
	key := path.Join(m.Prefix, "subnets", MakeSubnetKey(sn))
	value, err := json.Marshal(attrs)
	if err != nil {
		return err
	}

	opts := &client.SetOptions{
		PrevExist: client.PrevNoExist,
		TTL:       time.Hour * 24,
	}

	resp, err := m.cli.Set(ctx, key, string(value), opts)
	if err != nil {
		return err
	}

	if resp.Node.Expiration != nil {
		return fmt.Errorf("key expired")
	}
	return nil
}

func handleSubnets(ctx context.Context, snAttrs []*Attrs, dev *vxlanDevice) {

}
