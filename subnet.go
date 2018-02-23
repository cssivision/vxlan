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
	eventAdd    = "add"
)

type IP4 uint

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

// similar to net.IPNet but has uint based representation
type IP4Net struct {
	IP        IP4
	PrefixLen uint
}

func (n IP4Net) ToIPNet() *net.IPNet {
	return &net.IPNet{
		IP:   n.IP.ToIP(),
		Mask: net.CIDRMask(int(n.PrefixLen), 32),
	}
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

type Event struct {
	Type   string
	Subnet IP4Net
	Attrs  Attrs
}

type subnetWatcher struct {
	Subnet *IP4Net
}

func (sw *subnetWatcher) update(evts []Event) []Event {
	batch := []Event{}

	for _, e := range evts {
		if sw.Subnet != nil && e.Subnet.IP == sw.Subnet.IP && e.Subnet.PrefixLen == sw.Subnet.PrefixLen {
			continue
		}

		batch = append(batch, e)
	}

	return batch
}

func watchSubnets(ctx context.Context, sm *manager, ownSn *IP4Net, receiver chan []Event) {
	var index *uint64

	sw := subnetWatcher{
		Subnet: ownSn,
	}

	for {
		var evts []Event
		var err error
		evts, index, err = sm.watchEvents(ctx, index)
		if err != nil {
			logrus.Errorf("Watch subnets: %v", err)
			time.Sleep(time.Second)
			continue
		}

		var batch []Event
		if len(evts) > 0 {
			batch = sw.update(evts)
		}

		if len(batch) > 0 {
			receiver <- batch
		}
	}
}

func (m *manager) watchEvents(ctx context.Context, index *uint64) ([]Event, *uint64, error) {
	if index == nil {
		return m.getSubnets(ctx)
	}

	evt, idx, err := m.watchSubnets(ctx, index)
	if err != nil {
		return nil, nil, err
	}

	return []Event{evt}, &idx, nil
}

func (m *manager) watchSubnets(ctx context.Context, since *uint64) (Event, uint64, error) {
	key := path.Join(m.Prefix, "subnets")
	opts := &client.WatcherOptions{
		AfterIndex: *since,
		Recursive:  true,
	}

	e, err := m.cli.Watcher(key, opts).Next(ctx)
	if err != nil {
		return Event{}, 0, err
	}

	evt, err := parseSubnetWatchResponse(e)
	return evt, e.Node.ModifiedIndex, err
}

func (m *manager) getSubnets(ctx context.Context) ([]Event, *uint64, error) {
	key := path.Join(m.Prefix, "subnets")
	resp, err := m.cli.Get(ctx, key, &client.GetOptions{Recursive: true, Quorum: true})
	if err != nil {
		if etcdErr, ok := err.(client.Error); ok && etcdErr.Code == client.ErrorCodeKeyNotFound {
			return []Event{}, nil, nil
		}
		return nil, nil, err
	}

	evts := []Event{}

	for _, node := range resp.Node.Nodes {
		l, err := nodeToEvent(node)
		if err != nil {
			logrus.Warningf("Ignoring bad subnet node: %v", err)
			continue
		}

		evts = append(evts, *l)
	}

	return evts, &resp.Index, nil
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

func nodeToEvent(node *client.Node) (*Event, error) {
	sn := ParseSubnetKey(node.Key)
	if sn == nil {
		return nil, fmt.Errorf("failed to parse subnet key %s", node.Key)
	}

	attrs := &Attrs{}
	if err := json.Unmarshal([]byte(node.Value), attrs); err != nil {
		return nil, err
	}

	evt := Event{
		Type:   eventAdd,
		Attrs:  *attrs,
		Subnet: attrs.Subnet,
	}

	return &evt, nil
}

func parseSubnetWatchResponse(resp *client.Response) (Event, error) {
	sn := ParseSubnetKey(resp.Node.Key)
	if sn == nil {
		return Event{}, fmt.Errorf("%v %q: not a subnet, skipping", resp.Action, resp.Node.Key)
	}

	switch resp.Action {
	case "delete", "expire":
		return Event{}, fmt.Errorf("%v %q: not support, skipping", resp.Action, resp.Node.Key)

	default:
		attrs := &Attrs{}
		err := json.Unmarshal([]byte(resp.Node.Value), attrs)
		if err != nil {
			return Event{}, err
		}

		evt := Event{
			Type:   eventAdd,
			Subnet: *sn,
			Attrs:  *attrs,
		}
		return evt, nil
	}
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

func handleSubnets(ctx context.Context, sn IP4Net, sm *manager, dev *vxlanDevice) {
	evts := make(chan []Event)
	go func() {
		watchSubnets(ctx, sm, &sn, evts)
		logrus.Info("watch subnets exit")
	}()

	for evtBatch := range evts {
		dev.handleSubnetEvents(evtBatch)
	}
}
