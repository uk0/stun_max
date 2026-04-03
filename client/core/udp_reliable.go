package core

import (
	"encoding/base64"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Reliable UDP Transport (RUTP)
// Packet: [2B 0x534D][1B type][4B seq][2B len][data][2B cksum][2B 0x4D53]
// Strategy: send via UDP, if no ACK in 200ms → resend via relay (guaranteed delivery).

const (
	rutpMagicHead  = 0x534D
	rutpMagicTail  = 0x4D53
	rutpDATA       = 1
	rutpACK        = 2
	rutpOverhead   = 13 // 2+1+4+2 header + 2+2 tail
	rutpFallbackMs = 200
)

type rutpSender struct {
	conn     *net.UDPConn
	addr     *net.UDPAddr
	prefix   []byte // "TF:" + tunnelID
	seq      uint32
	pending  sync.Map // seq → *rutpPendingPkt
	client   *Client  // for relay fallback
	peerID   string
	tunnelID string
	done     chan struct{}
}

type rutpPendingPkt struct {
	payload []byte // original compressed data (for relay fallback)
	frame   []byte // full UDP frame (for UDP retransmit)
	sent    time.Time
	retries int32
	acked   int32
}

type rutpReceiver struct {
	conn   *net.UDPConn
	addr   *net.UDPAddr
	prefix []byte
	seen   sync.Map // seq → bool
}

func newRutpSender(conn *net.UDPConn, addr *net.UDPAddr, prefix []byte, client *Client, peerID, tunnelID string) *rutpSender {
	s := &rutpSender{
		conn: conn, addr: addr, prefix: prefix,
		client: client, peerID: peerID, tunnelID: tunnelID,
		done: make(chan struct{}),
	}
	go s.fallbackLoop()
	return s
}

func (s *rutpSender) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// Send sends data via UDP with RUTP framing. If ACK not received, relay fallback kicks in.
func (s *rutpSender) Send(data []byte) {
	seq := atomic.AddUint32(&s.seq, 1)

	// Build RUTP frame
	frame := rutpBuildFrame(rutpDATA, seq, data)
	msg := make([]byte, len(s.prefix)+len(frame))
	copy(msg, s.prefix)
	copy(msg[len(s.prefix):], frame)

	// Store for retransmit and relay fallback
	s.pending.Store(seq, &rutpPendingPkt{
		payload: append([]byte{}, data...),
		frame:   msg,
		sent:    time.Now(),
	})

	// Send via UDP
	s.conn.WriteToUDP(msg, s.addr)
}

func (s *rutpSender) OnACK(seq uint32) {
	if val, ok := s.pending.Load(seq); ok {
		val.(*rutpPendingPkt).acked = 1
		s.pending.Delete(seq)
	}
}

// fallbackLoop: retransmit unacked packets.
// Strategy: UDP retransmit twice (at 150ms, 300ms), then relay fallback (at 450ms).
func (s *rutpSender) fallbackLoop() {
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		now := time.Now()
		s.pending.Range(func(key, val any) bool {
			p := val.(*rutpPendingPkt)
			if atomic.LoadInt32(&p.acked) != 0 {
				s.pending.Delete(key)
				return true
			}
			age := now.Sub(p.sent)
			retries := atomic.LoadInt32(&p.retries)

			if retries < 2 && age > time.Duration(150*(retries+1))*time.Millisecond {
				// UDP retransmit (attempt 1 at 150ms, attempt 2 at 300ms)
				atomic.AddInt32(&p.retries, 1)
				s.conn.WriteToUDP(p.frame, s.addr)
			} else if retries == 2 && age > 450*time.Millisecond {
				// Final fallback: relay (guaranteed delivery)
				atomic.AddInt32(&p.retries, 1)
				encoded := base64.StdEncoding.EncodeToString(p.payload)
				s.client.sendRelay(s.peerID, "tunnel_data", TunnelData{
					TunnelID: s.tunnelID,
					Data:     encoded,
				})
				s.pending.Delete(key)
			} else if retries > 2 {
				s.pending.Delete(key)
			}
			return true
		})
	}
}

func newRutpReceiver(conn *net.UDPConn, addr *net.UDPAddr, prefix []byte) *rutpReceiver {
	return &rutpReceiver{conn: conn, addr: addr, prefix: prefix}
}

// OnData processes received DATA. Returns payload if new, nil if duplicate.
func (r *rutpReceiver) OnData(seq uint32, payload []byte) []byte {
	// Send ACK (send twice for redundancy)
	ack := rutpBuildFrame(rutpACK, seq, nil)
	msg := make([]byte, len(r.prefix)+len(ack))
	copy(msg, r.prefix)
	copy(msg[len(r.prefix):], ack)
	r.conn.WriteToUDP(msg, r.addr)
	r.conn.WriteToUDP(msg, r.addr) // redundant ACK

	// Dedup
	if _, dup := r.seen.LoadOrStore(seq, true); dup {
		return nil
	}
	// Periodic cleanup
	if seq%500 == 0 {
		cutoff := seq - 5000
		r.seen.Range(func(k, v any) bool {
			if k.(uint32) < cutoff {
				r.seen.Delete(k)
			}
			return true
		})
	}
	return payload
}

func rutpBuildFrame(typ byte, seq uint32, data []byte) []byte {
	n := len(data)
	frame := make([]byte, rutpOverhead+n)
	binary.BigEndian.PutUint16(frame[0:2], rutpMagicHead)
	frame[2] = typ
	binary.BigEndian.PutUint32(frame[3:7], seq)
	binary.BigEndian.PutUint16(frame[7:9], uint16(n))
	if n > 0 {
		copy(frame[9:9+n], data)
	}
	var ck uint32
	for i := 2; i < 9+n; i++ {
		ck += uint32(frame[i])
	}
	binary.BigEndian.PutUint16(frame[9+n:11+n], uint16(ck&0xFFFF))
	binary.BigEndian.PutUint16(frame[11+n:13+n], rutpMagicTail)
	return frame
}

func rutpParseFrame(frame []byte) (typ byte, seq uint32, payload []byte, ok bool) {
	if len(frame) < rutpOverhead {
		return 0, 0, nil, false
	}
	if binary.BigEndian.Uint16(frame[0:2]) != rutpMagicHead {
		return 0, 0, nil, false
	}
	typ = frame[2]
	seq = binary.BigEndian.Uint32(frame[3:7])
	n := int(binary.BigEndian.Uint16(frame[7:9]))
	if len(frame) < rutpOverhead+n {
		return 0, 0, nil, false
	}
	tail := 9 + n
	if binary.BigEndian.Uint16(frame[tail+2:tail+4]) != rutpMagicTail {
		return 0, 0, nil, false
	}
	var ck uint32
	for i := 2; i < tail; i++ {
		ck += uint32(frame[i])
	}
	if binary.BigEndian.Uint16(frame[tail:tail+2]) != uint16(ck&0xFFFF) {
		return 0, 0, nil, false
	}
	if n > 0 {
		payload = frame[9 : 9+n]
	}
	return typ, seq, payload, true
}
