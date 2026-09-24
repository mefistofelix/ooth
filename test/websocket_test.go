package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Test-only client for bounded RFC6455 frames, with no new module dependency.
type testWebSocket struct {
	connection net.Conn
	reader     *bufio.Reader
}

func openTestWebSocket(t *testing.T, address string) testWebSocket {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	connection.SetDeadline(time.Now().Add(8 * time.Second))
	fmt.Fprintf(connection, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", address)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("WebSocket upgrade response: %v", err)
	}
	if response.StatusCode != 101 || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("WebSocket upgrade: %s %v", response.Status, response.Header)
	}
	return testWebSocket{connection, reader}
}

func (socket testWebSocket) send(t *testing.T, opcode byte, payload []byte) {
	t.Helper()
	if len(payload) > 125 {
		t.Fatal("test frame too large")
	}
	frame := append([]byte{0x80 | opcode, 0x80 | byte(len(payload)), 1, 2, 3, 4}, payload...)
	for index := range payload {
		frame[index+6] ^= byte(index%4 + 1)
	}
	if _, err := socket.connection.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func (socket testWebSocket) receive(t *testing.T) (byte, []byte) {
	t.Helper()
	var header [2]byte
	if _, err := io.ReadFull(socket.reader, header[:]); err != nil {
		t.Fatalf("WebSocket frame header: %v", err)
	}
	if header[0]&128 == 0 || header[1]&128 != 0 || header[1]&127 > 125 {
		t.Fatalf("unexpected WebSocket frame: %x", header)
	}
	payload := make([]byte, header[1]&127)
	if _, err := io.ReadFull(socket.reader, payload); err != nil {
		t.Fatal(err)
	}
	return header[0] & 15, payload
}

func (socket testWebSocket) echo(t *testing.T) {
	t.Helper()
	for _, frame := range []struct {
		opcode byte
		data   []byte
	}{{1, []byte("ooth websocket")}, {2, []byte{0, 1, 255}}, {9, []byte("ping")}} {
		t.Logf("WebSocket sending opcode=%d", frame.opcode)
		socket.send(t, frame.opcode, frame.data)
		opcode, payload := socket.receive(t)
		want := frame.opcode
		if want == 9 {
			want = 10
		}
		if opcode != want || !bytes.Equal(payload, frame.data) {
			t.Fatalf("WebSocket echo opcode=%d payload=%q", opcode, payload)
		}
	}
}

func (socket testWebSocket) close(t *testing.T, serverInitiated bool) {
	t.Helper()
	t.Logf("WebSocket close server_initiated=%t", serverInitiated)
	payload := []byte{3, 232}
	if serverInitiated {
		opcode, data := socket.receive(t)
		if opcode != 8 || len(data) < 2 || binary.BigEndian.Uint16(data) != 1001 {
			t.Fatalf("expected Going Away, got %d %x", opcode, data)
		}
		payload = data
	}
	socket.send(t, 8, payload)
	if !serverInitiated {
		opcode, data := socket.receive(t)
		if opcode != 8 || len(data) < 2 || binary.BigEndian.Uint16(data) != 1000 {
			t.Fatalf("expected Normal Close, got %d %x", opcode, data)
		}
	}
	socket.connection.Close()
}
