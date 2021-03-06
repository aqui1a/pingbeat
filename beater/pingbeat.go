package beater

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/publisher"
	"github.com/joshuar/pingbeat/config"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"gopkg.in/go-playground/pool.v3"
)

const pingTimeout = 4 * time.Second

// Pingbeat contains configuration details
type Pingbeat struct {
	done        chan struct{}
	config      config.Config
	client      publisher.Client
	ipv4network string
	ipv6network string
	targets     map[string]Target
}

// PingInfo contains details about active ping requests/replies
type PingInfo struct {
	ID         int
	Seq        int
	Target     string
	Sent       time.Time
	Received   time.Time
	RTT        time.Duration
	Loss       bool
	LossReason string
}

// New creates a new Pingbeat beater struct
func New(b *beat.Beat, cfg *common.Config) (beat.Beater, error) {
	config := config.DefaultConfig
	if err := cfg.Unpack(&config); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}

	bt := &Pingbeat{
		done:   make(chan struct{}),
		config: config,
	}

	// Use privileged (i.e. raw socket) ping by default, else use a UDP ping
	if bt.config.Privileged {
		if os.Getuid() != 0 {
			return nil, fmt.Errorf("privileged specified but not running with privileges")
		}
		bt.ipv4network = "ip4:icmp"
		bt.ipv6network = "ip6:ipv6-icmp"
	} else {
		bt.ipv4network = "udp4"
		bt.ipv6network = "udp6"
	}

	// Fill the IPv4/IPv6 targets maps
	bt.targets = NewTargets(bt.config.Targets, bt.config.Privileged, bt.config.UseIPv4, bt.config.UseIPv6)
	return bt, nil
}

// Run is the main loop which sends/recieves ICMP messages and cleans up stale
// requests
func (bt *Pingbeat) Run(b *beat.Beat) error {
	logp.Info("pingbeat is running! Hit CTRL-C to stop it.")

	bt.client = b.Publisher.Connect()

	// Set up send/receive pools
	spool := pool.NewLimited(uint(len(bt.targets)) * uint(pingTimeout.Seconds()))
	defer spool.Close()

	// Set up a ticker to loop for the period specified
	ticker := time.NewTicker(bt.config.Period)
	defer ticker.Stop()
	timeout := time.NewTicker(pingTimeout)
	defer timeout.Stop()

	// Create a new global state to track active ping requests
	state := NewPingState()

	// Start receivers to capture incoming ping replies
	// Create required connections
	var ipv4conn, ipv6conn *icmp.PacketConn
	var err error
	var pingID = os.Getpid() & 0xffff
	logp.Debug("pingbeat", "pingID: %v", pingID)
	if bt.config.UseIPv4 {
		if ipv4conn, err = createConn(bt.ipv4network, "0.0.0.0"); err != nil {
			logp.Err("Error creating %s connection: %v", bt.ipv4network, err)
			return nil
		}
		logp.Info("Using %s connection", bt.ipv4network)
		go RecvPings(pingID, bt, state, ipv4conn)
	}
	if bt.config.UseIPv6 {
		if ipv6conn, err = createConn(bt.ipv6network, "::"); err != nil {
			logp.Err("Error creating %s connection: %v", bt.ipv6network, err)
			return nil
		}
		logp.Info("Using %s connection", bt.ipv6network)
		go RecvPings(pingID, bt, state, ipv6conn)
	}

	for {
		select {
		case <-bt.done:
			return nil
		case <-timeout.C:
			// Timeout reached, clean up any pending ping requests where there
			// has been no response
			go state.CleanPings(pingTimeout)
		case <-ticker.C:
			// Batch queue echo request
			sendBatch := spool.Batch()
			go func(*icmp.PacketConn, *icmp.PacketConn) {
				for ip, target := range bt.targets {
					if net.ParseIP(ip).To4() != nil {
						sendBatch.Queue(SendPing(ipv4conn, pingTimeout, state.GetSeqNo(), target.Addr))
					} else {
						sendBatch.Queue(SendPing(ipv6conn, pingTimeout, state.GetSeqNo(), target.Addr))
					}
				}
				sendBatch.QueueComplete()
			}(ipv4conn, ipv6conn)

			// For each successfully sent echo request
			for result := range sendBatch.Results() {
				// Grab info of the sent request
				if result.Value() == nil {
					logp.Debug("pingbeat", "Send unsuccessful: %v", result.Error())
					break
				}
				info := result.Value().(*PingInfo)
				if err := result.Error(); err != nil {
					logp.Debug("pingbeat", "Send unsuccessful: %v", err)
				}
				success := state.AddPing(info.Target, info.Seq, info.Sent)
				if !success {
					logp.Err("Error adding ping (%v:%v) to state", info.Seq, info.Target)
				}
			}
		}
	}
}

// Stop cleans up Pingbeat
func (bt *Pingbeat) Stop() {
	bt.client.Close()
	close(bt.done)
}

// RecvPings listens for ICMP messages, decodes them into the right type and
// checks if they were sent by this Pingbeat, before processing them
func RecvPings(myID int, bt *Pingbeat, state *PingState, conn *icmp.PacketConn) {
	for {
		// Based on the connection, work out whether we are dealing with
		// IPv4 or IPv6 ICMP messages
		var pingType icmp.Type
		switch {
		case conn.IPv4PacketConn() != nil:
			pingType = ipv4.ICMPTypeEcho
		case conn.IPv4PacketConn() != nil:
			pingType = ipv6.ICMPTypeEchoRequest
		default:
			err := errors.New("Unknown connection type")
			logp.Err("Error parsing connection: %v", err)
			break
		}

		// Read data from the connection
		bd := make([]byte, 1500)
		n, peer, err := conn.ReadFrom(bd)
		if err != nil {
			logp.Err("Couldn't read from connection: %v", err)
			continue
		}
		var target string
		switch peer.(type) {
		case *net.UDPAddr:
			target, _, _ = net.SplitHostPort(peer.String())
		case *net.IPAddr:
			target = peer.String()
		default:
			logp.Err("Error parsing received address %v", target)
			continue
		}

		if n == 0 {
			continue
		}
		// Parse the data into an ICMP message
		message, err := icmp.ParseMessage(pingType.Protocol(), bd[:n])
		if err != nil {
			logp.Err("Couldn't parse response: %v", err)
			continue
		}

		ping := &PingInfo{}
		// Switch for the ICMP message type
		switch message.Body.(type) {
		case *icmp.Echo:
			ping.Seq = message.Body.(*icmp.Echo).Seq
			ping.ID = message.Body.(*icmp.Echo).ID
			ping.Target = target
			ping.Loss = false
			ping.Received = time.Now().UTC()
		case *icmp.TimeExceeded:
			ping.Loss = true
			ping.LossReason = "Time Exceeded"
			ping.ID, ping.Seq, ping.Target = parseICMPError(message.Body.(*icmp.TimeExceeded).Data)
		case *icmp.PacketTooBig:
			ping.Loss = true
			ping.LossReason = "Packet Too Big"
			ping.ID, ping.Seq, ping.Target = parseICMPError(message.Body.(*icmp.PacketTooBig).Data)
		case *icmp.DstUnreach:
			ping.Loss = true
			ping.LossReason = "Destination Unreachable"
			ping.ID, ping.Seq, ping.Target = parseICMPError(message.Body.(*icmp.DstUnreach).Data)
		default:
		}
		if ping.ID != 0 && ping.ID != myID {
			logp.Debug("RecvPings", "Ping response from %v not from me:", target)
		} else {
			if !ping.Loss {
				ping.RTT = state.CalcPingRTT(ping.Seq, ping.Received)
			} else {
				logp.Warn("%v: %v", ping.LossReason, ping.Target)
			}
			go bt.ProcessPing(ping)
			state.DelPing(ping.Seq)
		}
	}
}

// SendPing sends an ICMP EchoRequest packet to with provided sequence number to
// the provided target through the given connection
func SendPing(conn *icmp.PacketConn, timeout time.Duration, seq int, addr net.Addr) pool.WorkFunc {
	return func(wu pool.WorkUnit) (interface{}, error) {
		if wu.IsCancelled() {
			logp.Debug("SendPings", "SendPing: workunit cancelled")
			return nil, nil
		}
		// Based on the connection, work out whether we are dealing with
		// IPv4 or IPv6 ICMP messages
		var pingType icmp.Type
		switch {
		case conn.IPv4PacketConn() != nil:
			pingType = ipv4.ICMPTypeEcho
		case conn.IPv4PacketConn() != nil:
			pingType = ipv6.ICMPTypeEchoRequest
		default:
			err := errors.New("Unknown connection type")
			return nil, err
		}

		// Create an ICMP Echo Request
		var id = os.Getpid() & 0xffff
		message := &icmp.Message{
			Type: pingType, Code: 0,
			Body: &icmp.Echo{
				ID:   id,
				Seq:  seq,
				Data: []byte("pingbeat: y'know, for pings!"),
			},
		}
		// Marshall the Echo request for sending via a connection
		binary, err := message.Marshal(nil)
		if err != nil {
			return nil, err
		}
		var t string
		switch addr.(type) {
		case *net.UDPAddr:
			t, _, _ = net.SplitHostPort(addr.String())
		case *net.IPAddr:
			t = addr.String()
		default:
			err := errors.New("Unknown address type")
			return nil, err
		}

		ping := &PingInfo{
			Seq:    seq,
			Target: t,
		}
		// Send the request
		if _, err := conn.WriteTo(binary, addr); err != nil {
			return ping, err
		}
		ping.Sent = time.Now().UTC()
		return ping, nil
	}
}

// ProcessPing fetches the details of this ping from the current state
// and then creates an ping event to be published
func (bt *Pingbeat) ProcessPing(ping *PingInfo) {
	if _, found := bt.targets[ping.Target]; !found {
		logp.Err("No details for %v in targets!", ping.Target)
	} else {
		name := bt.targets[ping.Target].Name
		tags := bt.targets[ping.Target].Tags
		if ping.Loss {
			event := common.MapStr{
				"@timestamp": common.Time(time.Now().UTC()),
				"type":       "pingbeat",
				"target": common.MapStr{
					"name": name,
					"addr": ping.Target,
					"tags": tags,
				},
				"loss":   true,
				"reason": ping.LossReason,
			}
			go bt.client.PublishEvent(event)
			logp.Debug("ProcessPing", "Processed ping error for %v (%v): %v", name, ping.Target, ping.LossReason)
		} else {
			event := common.MapStr{
				"@timestamp": common.Time(time.Now().UTC()),
				"type":       "pingbeat",
				"target": common.MapStr{
					"name": name,
					"addr": ping.Target,
					"tags": tags,
				},
				"rtt": milliSeconds(ping.RTT),
			}
			go bt.client.PublishEvent(event)
			logp.Debug("ProcessPing", "Processed ping %v for %v (%v): %v", ping.Seq, name, ping.Target, ping.RTT)
		}
	}
}

func parseICMPError(data []byte) (int, int, string) {
	IPheader, err := ipv4.ParseHeader(data[:len(data)-8])
	if err != nil {
		logp.Err("parseICMPError", "Failed to parse packet header:", err)
	}
	ICMPHdr := data[IPheader.Len:]
	var ID, Seq uint16
	err = binary.Read(bytes.NewReader(ICMPHdr[6:8]), binary.BigEndian, &Seq)
	if err != nil {
		logp.Err("parseICMPError", "Failed to parse packet header:", err)
	}
	err = binary.Read(bytes.NewReader(ICMPHdr[4:6]), binary.BigEndian, &ID)
	if err != nil {
		logp.Err("parseICMPError", "Failed to parse packet header:", err)
	}
	return int(ID), int(Seq), IPheader.Dst.String()
}

func createConn(n string, a string) (*icmp.PacketConn, error) {
	c, err := icmp.ListenPacket(n, a)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func milliSeconds(d time.Duration) float64 {
	msec := d / time.Millisecond
	nsec := d % time.Millisecond
	return float64(msec) + float64(nsec)*1e-6
}
