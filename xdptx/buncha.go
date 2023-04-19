package main

import (
	"flag"
	"github.com/asavie/xdp"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/sys/unix"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

import (
	"github.com/vishvananda/netlink"
	klog "k8s.io/klog/v2"
)

var (
	flagIfName      = flag.String("ifname", "", "interface name")
	flagTarget      = flag.String("target", "", "target IP address")
	flagPayloadSize = flag.Int("size", 1, "payload size")
	flagNumQueues   = flag.Int("n", 1, "number of queues")
	flagFirstQueue  = flag.Int("first", 0, "first queue")
)

func init() {
	klog.InitFlags(nil)
	flag.Parse()
	if *flagIfName == "" {
		klog.Exit("missing required flag: -ifname")
	}
	if *flagTarget == "" {
		klog.Exit("missing required flag: -target")
	}

	xdp.DefaultSocketFlags = unix.XDP_ZEROCOPY
	xdp.DefaultXdpFlags = 0
}

// Inspired by https://github.com/asavie/xdp/blob/master/examples/sendudp/sendudp.go
func txer(queueID int) {
	// Resolve NIC
	link, err := netlink.LinkByName(*flagIfName)
	if err != nil {
		klog.Exitf("failed to resolve link %s: %v", *flagIfName, err)
	}

	program, err := xdp.NewProgram(queueID + 1)
	if err != nil {
		panic(err)
	}

	if err := program.Attach(link.Attrs().Index); err != nil {
		klog.Exitf("failed to attach program to %s: %v", *flagIfName, err)
	}

	// Create XDP socket
	xsk, err := xdp.NewSocket(link.Attrs().Index, queueID, &xdp.SocketOptions{
		NumFrames:              128,
		FrameSize:              2048,
		FillRingNumDescs:       64,
		CompletionRingNumDescs: 64,
		RxRingNumDescs:         64,
		TxRingNumDescs:         64,
	})

	if err := program.Register(queueID, xsk.FD()); err != nil {
		klog.Exitf("failed to register XDP program: %v", err)
	}

	klog.Infof("txer %d: xsk fd %d", queueID, xsk.FD())

	// Remove the XDP BPF program on interrupt.
	c := make(chan os.Signal)
	var done bool
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		done = true
		if err := program.Detach(link.Attrs().Index); err != nil {
			klog.Errorf("failed to detach program: %v", err)
		}
		os.Exit(1)
	}()

	if err != nil {
		klog.Exitf("failed to create XDP socket: %v", err)
	}

	// Ask kernel for route to target IP
	route, err := netlink.RouteGet(net.ParseIP(*flagTarget))
	if err != nil {
		panic(err)
	}

	// Bail if route is not via our link
	if route[0].LinkIndex != link.Attrs().Index {
		klog.Exitf("route to %s is not via %s", *flagTarget, *flagIfName)
	}
	// Bail if via is not a neighbor
	if route[0].Gw != nil {
		klog.Exitf("not implemented: route to %s is not via neighbor", *flagTarget)
	}

	// Get destination MAC from kernel
	ntt, err := netlink.NeighList(link.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		panic(err)
	}

	dstIP := net.ParseIP(*flagTarget)
	var dstMac net.HardwareAddr
	for _, n := range ntt {
		if n.IP.Equal(dstIP) {
			dstMac = n.HardwareAddr
		}
	}

	if dstMac == nil {
		klog.Exitf("failed to resolve MAC for %s", *flagTarget)
	}

	klog.V(1).Infof("destination route: %+v", route)
	klog.Infof("source mac on %s is %s", *flagIfName, link.Attrs().HardwareAddr.String())
	klog.Infof("destination mac on %s is %s", *flagIfName, dstMac.String())
	klog.Infof("source IP is %s", route[0].Src.String())
	klog.Infof("destination IP is %s", dstIP.String())

	// Generate packet headers
	eth := &layers.Ethernet{
		SrcMAC:       link.Attrs().HardwareAddr,
		DstMAC:       dstMac,
		EthernetType: layers.EthernetTypeIPv4,
	}

	ip := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Id:       0,
		Protocol: layers.IPProtocolUDP,
		SrcIP:    route[0].Src,
		DstIP:    dstIP,
	}

	srcPort := rand.Intn(math.MaxUint16)

	udp := &layers.UDP{
		SrcPort: layers.UDPPort(srcPort),
		DstPort: layers.UDPPort(5555),
	}

	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		panic(err)
	}

	payload := make([]byte, *flagPayloadSize)
	for i := 0; i < len(payload); i++ {
		payload[i] = byte(i)
	}

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	err = gopacket.SerializeLayers(buf, opts, eth, ip, udp, gopacket.Payload(payload))
	if err != nil {
		panic(err)
	}

	frameLen := len(buf.Bytes())

	// Fill all the frames in UMEM with the pre-generated UDP packet.

	descs := xsk.GetDescs(math.MaxInt32, false)
	for i := range descs {
		frameLen = copy(xsk.GetFrame(descs[i]), buf.Bytes())
	}

	klog.Infof("sending %d byte UDP packets from %v (%v) to %v (%v)...\n",
		frameLen, ip.SrcIP, eth.SrcMAC, ip.DstIP, eth.DstMAC)

	go func() {
		var err error
		var prev xdp.Stats
		var cur xdp.Stats
		var numPkts uint64
		for i := uint64(0); ; i++ {
			time.Sleep(time.Duration(1) * time.Second)
			cur, err = xsk.Stats()
			if err != nil {
				panic(err)
			}
			numPkts = cur.Completed - prev.Completed
			klog.Infof("[%d] %d packets/s (%d Mb/s)\n", queueID, numPkts, (numPkts*uint64(frameLen)*8)/(1000*1000))
			prev = cur
		}
	}()

	for {
		descs := xsk.GetDescs(xsk.NumFreeTxSlots(), false)
		for i := range descs {
			descs[i].Len = uint32(frameLen)
		}
		if done {
			continue
		}
		xsk.Transmit(descs)
		_, _, err = xsk.Poll(-1)
		if err != nil {
			panic(err)
		}
	}
}

func main() {
	klog.Infof("launching %d txers", *flagNumQueues)
	wg := sync.WaitGroup{}
	for i := 0; i < *flagNumQueues; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			txer(i + *flagFirstQueue)
		}()
	}
	wg.Wait()
}
