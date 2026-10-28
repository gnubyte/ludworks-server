package airplay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func readResp(s string) (*message, error) {
	return readMessage(bufio.NewReaderSize(strings.NewReader(s), maxHeaderLine), true)
}

func TestReadMessage(t *testing.T) {
	m, err := readResp("RTSP/1.0 200 OK\r\nCSeq: 3\r\nAudio-Latency: 11025\r\nContent-Length: 5\r\n\r\nhelloEXTRA")
	if err != nil || m.status != 200 || m.reason != "OK" || m.get("audio-latency") != "11025" || string(m.body) != "hello" {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = readResp("HTTP/1.1 200 OK\nTransfer-Encoding: chunked\n\n3\r\nabc\r\n2;ext=1\r\nde\r\n0\r\nX-Trailer: 1\r\n\r\n")
	if err != nil || string(m.body) != "abcde" {
		t.Fatalf("chunked: %q %v", m.body, err)
	}
	m, err = readResp("HTTP/1.1 470 Connection Authorization Required\r\n\r\n")
	if err != nil || m.status != 470 || len(m.body) != 0 {
		t.Fatalf("470: %+v %v", m, err)
	}
	req, err := readMessage(bufio.NewReader(strings.NewReader("POST /command RTSP/1.0\r\nCSeq: 9\r\nContent-Length: 2\r\n\r\nhi")), false)
	if err != nil || req.method != "POST" || req.uri != "/command" || req.get("cseq") != "9" || string(req.body) != "hi" {
		t.Fatalf("request: %+v %v", req, err)
	}
}

func TestReadMessageHostile(t *testing.T) {
	cases := map[string]string{
		"bad proto":        "SIP/2.0 200 OK\r\n\r\n",
		"bad status":       "RTSP/1.0 2x0 OK\r\n\r\n",
		"no colon":         "RTSP/1.0 200 OK\r\nbroken\r\n\r\n",
		"space in name":    "RTSP/1.0 200 OK\r\nBad Name: x\r\n\r\n",
		"long line":        "RTSP/1.0 200 OK\r\nX: " + strings.Repeat("a", 5000) + "\r\n\r\n",
		"too many headers": "RTSP/1.0 200 OK\r\n" + strings.Repeat("X: y\r\n", 65) + "\r\n",
		"header too large": "RTSP/1.0 200 OK\r\n" + strings.Repeat("X-Long-Header: "+strings.Repeat("v", 3000)+"\r\n", 6) + "\r\n",
		"huge body":        "RTSP/1.0 200 OK\r\nContent-Length: 2000000\r\n\r\n",
		"negative length":  "RTSP/1.0 200 OK\r\nContent-Length: -1\r\n\r\n",
		"short body":       "RTSP/1.0 200 OK\r\nContent-Length: 10\r\n\r\nabc",
		"bad chunk":        "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n",
		"huge chunks":      "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + "fffff\r\n" + strings.Repeat("a", 0xfffff) + "\r\nfffff\r\n",
		"eof in headers":   "RTSP/1.0 200 OK\r\nCSeq: 1\r\n",
	}
	for name, in := range cases {
		if _, err := readResp(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeRTSPServer answers each request on one connection with respond(req).
func fakeRTSPServer(t *testing.T, respond func(req *textproto.Reader, w *bufio.Writer, first string) bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tp := textproto.NewReader(bufio.NewReader(c))
				w := bufio.NewWriter(c)
				for {
					line, err := tp.ReadLine()
					if err != nil {
						return
					}
					if !respond(tp, w, line) {
						return
					}
					w.Flush()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func dialTest(t *testing.T, addr string) *msgConn {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	var p int
	fmt.Sscan(port, &p)
	c, err := dialMsgConn(context.Background(), host, p, []hdr{{"User-Agent", "AirPlay/test"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.close() })
	return c
}

func TestMsgConnExchange(t *testing.T) {
	addr := fakeRTSPServer(t, func(tp *textproto.Reader, w *bufio.Writer, first string) bool {
		h, err := tp.ReadMIMEHeader()
		if err != nil {
			return false
		}
		body := ""
		if n := h.Get("Content-Length"); n != "" {
			var l int
			fmt.Sscan(n, &l)
			b := make([]byte, l)
			if _, err := io.ReadFull(tp.R, b); err != nil {
				return false
			}
			body = string(b)
		}
		// A stale response with an old CSeq first, then the real one.
		fmt.Fprintf(w, "RTSP/1.0 200 OK\r\nCSeq: 999\r\n\r\n")
		fmt.Fprintf(w, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nX-Echo: %s|%s|%s\r\nContent-Length: %d\r\n\r\n%s",
			h.Get("CSeq"), first, h.Get("User-Agent"), h.Get("Content-Type"), len(body), body)
		return true
	})
	c := dialTest(t, addr)
	for i := 1; i <= 3; i++ {
		resp, err := c.do(context.Background(), "SET_PARAMETER", "rtsp://h/1", rtspProto,
			[]hdr{{"Content-Type", "text/parameters"}}, []byte("volume: -10.0\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := "SET_PARAMETER rtsp://h/1 RTSP/1.0|AirPlay/test|text/parameters"
		if resp.get("CSeq") != fmt.Sprint(i) || resp.get("X-Echo") != want || string(resp.body) != "volume: -10.0\r\n" {
			t.Fatalf("exchange %d: %+v", i, resp)
		}
	}
}

func TestMsgConnTimeoutsAndDeadConnection(t *testing.T) {
	addr := fakeRTSPServer(t, func(tp *textproto.Reader, w *bufio.Writer, first string) bool {
		tp.ReadMIMEHeader()
		time.Sleep(2 * time.Second) // never answers in time
		return false
	})
	c := dialTest(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.do(ctx, "OPTIONS", "*", rtspProto, nil, nil)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if c.alive() {
		t.Error("connection still alive after a failed exchange")
	}
	if _, err := c.do(context.Background(), "OPTIONS", "*", rtspProto, nil, nil); !errors.Is(err, errConnDead) {
		t.Errorf("second exchange: %v", err)
	}

	// Cancelling the context aborts a blocked exchange.
	c2 := dialTest(t, addr)
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel2)
	if _, err := c2.do(ctx2, "OPTIONS", "*", rtspProto, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled exchange: %v", err)
	}
}

func TestWriteMessage(t *testing.T) {
	var b bytes.Buffer
	writeMessage(&b, &message{method: "POST", uri: "/feedback", proto: rtspProto, header: []hdr{{"CSeq", "4"}}})
	if b.String() != "POST /feedback RTSP/1.0\r\nCSeq: 4\r\nContent-Length: 0\r\n\r\n" {
		t.Errorf("%q", b.String())
	}
	b.Reset()
	writeMessage(&b, &message{method: "OPTIONS", uri: "*", proto: rtspProto})
	if b.String() != "OPTIONS * RTSP/1.0\r\n\r\n" {
		t.Errorf("%q", b.String())
	}
	b.Reset()
	writeMessage(&b, &message{proto: rtspProto, status: 200, reason: "OK"})
	if b.String() != "RTSP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n" {
		t.Errorf("%q", b.String())
	}
}

func FuzzReadMessage(f *testing.F) {
	f.Add([]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 3\r\n\r\nabc"), true)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n"), true)
	f.Add([]byte("POST /command RTSP/1.0\r\nContent-Length: 0\r\n\r\n"), false)
	f.Fuzz(func(t *testing.T, b []byte, response bool) {
		m, err := readMessage(bufio.NewReaderSize(bytes.NewReader(b), maxHeaderLine), response)
		if err != nil {
			return
		}
		if len(m.body) > maxBodyBytes || len(m.header) > maxHeaders {
			t.Fatalf("limits exceeded: %d body bytes, %d headers", len(m.body), len(m.header))
		}
	})
}
