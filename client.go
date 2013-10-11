// Copyright 2011 Miek Gieben. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

// A client implementation.

import (
	"io"
	"net"
	"time"
)

const dnsTimeout time.Duration = 2 * 1e9

// A Conn represents a connection to a DNS server.
type Conn struct {
	net.Conn                         // a net.Conn holding the connection
	UDPSize        uint16            // Minimum receive buffer for UDP messages
	TsigSecret     map[string]string // Secret(s) for Tsig map[<zonename>]<base64 secret>, zonename must be fully qualified
	rtt            time.Duration
	t              time.Time
	tsigRequestMAC string
}

// A Client defines parameters for a DNS client. A nil Client is usable for sending queries.
type Client struct {
	Net            string            // if "tcp" a TCP query will be initiated, otherwise an UDP one (default is "" for UDP)
	DialTimeout    time.Duration     // net.DialTimeout (ns), defaults to 2 * 1e9
	ReadTimeout    time.Duration     // net.Conn.SetReadTimeout value for connections (ns), defaults to 2 * 1e9
	WriteTimeout   time.Duration     // net.Conn.SetWriteTimeout value for connections (ns), defaults to 2 * 1e9
	TsigSecret     map[string]string // secret(s) for Tsig map[<zonename>]<base64 secret>, zonename must be fully qualified
	SingleInflight bool              // if true suppress multiple outstanding queries for the same Qname, Qtype and Qclass
	group          singleflight
}

// Exchange performs a synchronous UDP query. It sends the message m to the address
// contained in a and waits for an reply.
func Exchange(m *Msg, a string) (r *Msg, err error) {
	co := new(Conn)
	co.Conn, err = net.DialTimeout("udp", a, dnsTimeout)
	if err != nil {
		return nil, err
	}

	defer co.Close()
	co.SetReadDeadline(time.Now().Add(dnsTimeout))
	co.SetWriteDeadline(time.Now().Add(dnsTimeout))
	if err = co.WriteMsg(m); err != nil {
		return nil, err
	}
	r, err = co.ReadMsg()
	return r, err
}

// Exchange performs an synchronous query. It sends the message m to the address
// contained in a and waits for an reply. Basic use pattern with a *dns.Client:
//
//	c := new(dns.Client)
//	in, rtt, err := c.Exchange(message, "127.0.0.1:53")
//
func (c *Client) Exchange(m *Msg, a string) (r *Msg, rtt time.Duration, err error) {
	if !c.SingleInflight {
		return c.exchange(m, a)
	}
	// This adds a bunch of garbage, TODO(miek).
	t := "nop"
	if t1, ok := TypeToString[m.Question[0].Qtype]; ok {
		t = t1
	}
	cl := "nop"
	if cl1, ok := ClassToString[m.Question[0].Qclass]; ok {
		cl = cl1
	}
	r, rtt, err, shared := c.group.Do(m.Question[0].Name+t+cl, func() (*Msg, time.Duration, error) {
		return c.exchange(m, a)
	})
	if err != nil {
		return r, rtt, err
	}
	if shared {
		r1 := r.copy()
		r = r1
	}
	return r, rtt, nil
}

func (c *Client) exchange(m *Msg, a string) (r *Msg, rtt time.Duration, err error) {
	co := new(Conn)
	timeout := dnsTimeout
	if c.DialTimeout != 0 {
		timeout = c.DialTimeout
	}
	if c.Net == "" {
		co.Conn, err = net.DialTimeout("udp", a, timeout)
	} else {
		co.Conn, err = net.DialTimeout(c.Net, a, timeout)
	}
	if err != nil {
		return nil, 0, err
	}
	timeout = dnsTimeout
	if c.ReadTimeout != 0 {
		timeout = c.ReadTimeout
	}
	co.SetReadDeadline(time.Now().Add(timeout))
	timeout = dnsTimeout
	if c.WriteTimeout != 0 {
		timeout = c.WriteTimeout
	}
	co.SetWriteDeadline(time.Now().Add(timeout))
	defer co.Close()
	opt := m.IsEdns0()
	if opt != nil && opt.UDPSize() >= MinMsgSize {
		co.UDPSize = opt.UDPSize()
	}
	co.TsigSecret = c.TsigSecret
	if err = co.WriteMsg(m); err != nil {
		return nil, 0, err
	}
	r, err = co.ReadMsg()
	return r, co.rtt, err
}

// ReadMsg reads a message from the connection co.
// If the received message contains a TSIG record the transaction
// signature is verified.
func (co *Conn) ReadMsg() (*Msg, error) {
	var p []byte
	m := new(Msg)
	if _, ok := co.Conn.(*net.TCPConn); ok {
		p = make([]byte, MaxMsgSize)
	} else {
		if co.UDPSize >= 512 {
			p = make([]byte, co.UDPSize)
		} else {
			p = make([]byte, MinMsgSize)
		}
	}
	n, err := co.Read(p)
	if err != nil && n == 0 {
		return nil, err
	}
	p = p[:n]
	if err := m.Unpack(p); err != nil {
		return nil, err
	}
	co.rtt = time.Since(co.t)
	if t := m.IsTsig(); t != nil {
		if _, ok := co.TsigSecret[t.Hdr.Name]; !ok {
			return m, ErrSecret
		}
		// Need to work on the original message p, as that was used to calculate the tsig.
		err = TsigVerify(p, co.TsigSecret[t.Hdr.Name], co.tsigRequestMAC, false)
	}
	return m, err
}

// Read implements the net.Conn read method.
func (co *Conn) Read(p []byte) (n int, err error) {
	if co.Conn == nil {
		return 0, ErrConnEmpty
	}
	if len(p) < 2 {
		return 0, io.ErrShortBuffer
	}
	if t, ok := co.Conn.(*net.TCPConn); ok {
		n, err = t.Read(p[0:2])
		if err != nil || n != 2 {
			return n, err
		}
		l, _ := unpackUint16(p[0:2], 0)
		if l == 0 {
			return 0, ErrShortRead
		}
		if int(l) > len(p) {
			return int(l), io.ErrShortBuffer
		}
		n, err = t.Read(p[:l])
		if err != nil {
			return n, err
		}
		i := n
		for i < int(l) {
			j, err := t.Read(p[i:int(l)])
			if err != nil {
				return i, err
			}
			i += j
		}
		n = i
		return n, err
	}
	// assume udp connection
	n, _, err = co.Conn.(*net.UDPConn).ReadFromUDP(p)
	if err != nil {
		return n, err
	}
	return n, err
}

// WriteMsg sends a message throught the connection co.
// If the message m contains a TSIG record the transaction
// signature is calculated.
func (co *Conn) WriteMsg(m *Msg) (err error) {
	var out []byte
	if t := m.IsTsig(); t != nil {
		mac := ""
		if _, ok := co.TsigSecret[t.Hdr.Name]; !ok {
			return ErrSecret
		}
		out, mac, err = TsigGenerate(m, co.TsigSecret[t.Hdr.Name], co.tsigRequestMAC, false)
		// Set for the next read, allthough only used in zone transfers
		co.tsigRequestMAC = mac
	} else {
		out, err = m.Pack()
	}
	if err != nil {
		return err
	}
	co.t = time.Now()
	if _, err = co.Write(out); err != nil {
		return err
	}
	return nil
}

// Write implements the net.Conn Write method.
func (co *Conn) Write(p []byte) (n int, err error) {
	if t, ok := co.Conn.(*net.TCPConn); ok {
		if len(p) < 2 {
			return 0, io.ErrShortBuffer
		}
		l := make([]byte, 2)
		l[0], l[1] = packUint16(uint16(len(p)))
		p = append(l, p...)
		n, err := t.Write(p)
		if err != nil {
			return n, err
		}
		i := n
		if i < len(p) {
			j, err := t.Write(p[i:len(p)])
			if err != nil {
				return i, err
			}
			i += j
		}
		n = i
		return n, err
	}
	n, err = co.Conn.(*net.UDPConn).Write(p)
	return n, err
}

// Close implements the net.Conn Close method.
func (co *Conn) Close() error { return co.Conn.Close() }

// LocalAddr implements the net.Conn LocalAddr method.
func (co *Conn) LocalAddr() net.Addr { return co.Conn.LocalAddr() }

// RemoteAddr implements the net.Conn RemoteAddr method.
func (co *Conn) RemoteAddr() net.Addr { return co.Conn.RemoteAddr() }

// SetDeadline implements the net.Conn SetDeadline method.
func (co *Conn) SetDeadline(t time.Time) error { return co.Conn.SetDeadline(t) }

// SetReadDeadline implements the net.Conn SetReadDeadline method.
func (co *Conn) SetReadDeadline(t time.Time) error { return co.Conn.SetReadDeadline(t) }

// SetWriteDeadline implements the net.Conn SetWriteDeadline method.
func (co *Conn) SetWriteDeadline(t time.Time) error { return co.Conn.SetWriteDeadline(t) }
