// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fcgi implements the FastCGI protocol.
//
// See https://fast-cgi.github.io/ for an unofficial mirror of the
// original documentation.
//
// Currently only the responder role is supported.
package fcgi

// This file defines the raw protocol and some utilities used by the child and
// the host.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// recType is a record type, as defined by
// https://web.archive.org/web/20150420080736/http://www.fastcgi.com/drupal/node/6?q=node/22#S8
type RecType uint8

const (
	RecTypeBeginRequest    RecType = 1
	RecTypeAbortRequest    RecType = 2
	RecTypeEndRequest      RecType = 3
	RecTypeParams          RecType = 4
	RecTypeStdin           RecType = 5
	RecTypeStdout          RecType = 6
	RecTypeStderr          RecType = 7
	RecTypeData            RecType = 8
	RecTypeGetValues       RecType = 9
	RecTypeGetValuesResult RecType = 10
	RecTypeUnknownType     RecType = 11
)

const (
	RecMaxWrite = 65535 // maximum record body
	RecMaxPad   = 255
)

const (
	RoleResponder = iota + 1 // only Responders are implemented.
	RoleAuthorizer
	RoleFilter
)

const (
	StatusRequestComplete = iota
	StatusCantMultiplex
	StatusOverloaded
	StatusUnknownRole
)

type RecordHeader struct {
	Version       uint8
	Type          recType
	Id            uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

type Record struct {
	H   RecordHeader
	Buf [maxWrite + maxPad]byte
	NextRecord *Record
}

type RecBeginRequestData struct {
	Role     uint16
	Flags    uint8
	Reserved [5]uint8
}

func (rec *Record) Read(r io.Reader) (err error) {
	if err = binary.Read(r, binary.BigEndian, &rec.H); err != nil {
		return err
	}
	if rec.H.Version != 1 {
		return errors.New("fcgi: invalid header version")
	}

	n := int(rec.H.ContentLength) + int(rec.H.PaddingLength)
	if _, err = io.ReadFull(r, rec.Buf[:n]); err != nil {
		return err
	}
	if r.H.Type == RecTypeParams && len(rec.content()) > 0 {
	
		if  {
			req.rawParams = append(req.rawParams, rec.content()...)
			return nil
		}
	return nil
}

func (r *record) Content() []byte {
	return r.Buf[:r.H.ContentLength]
}

func (r *Record) GetBeginRequestData(content []byte) (RecBeginRequestData, error) {
	content := r.Content()
	if r.H.Type != RecTypeBeginRequest {
		return nil, errors.New("fcgi: record is not of type RecTypeBeginRequest")
	}
	var br beginRequest
	if len(content) != 8 {
		return nil, errors.New("fcgi: invalid begin request record")
	}
	br.role = binary.BigEndian.Uint16(content)
	br.flags = content[2]
	return br, nil
}


func readSize(s []byte) (uint32, int) {
	if len(s) == 0 {
		return 0, 0
	}
	size, n := uint32(s[0]), 1
	if size&(1<<7) != 0 {
		if len(s) < 4 {
			return 0, 0
		}
		n = 4
		size = binary.BigEndian.Uint32(s)
		size &^= 1 << 31
	}
	return size, n
}

func readString(s []byte, size uint32) string {
	if size > uint32(len(s)) {
		return ""
	}
	return string(s[:size])
}

// parseParams reads an encoded []byte into Params.
func (r *Record) GetParamsData() string[][] {
	text := r.rawParams
	r.rawParams = nil
	for len(text) > 0 {
		keyLen, n := readSize(text)
		if n == 0 {
			return
		}
		text = text[n:]
		valLen, n := readSize(text)
		if n == 0 {
			return
		}
		text = text[n:]
		if int(keyLen)+int(valLen) > len(text) {
			return
		}
		key := readString(text, keyLen)
		text = text[keyLen:]
		val := readString(text, valLen)
		text = text[valLen:]
		r.params[key] = val
	}
}








/*





// keep the connection between web-server and responder open after request
const flagKeepConn = 1


// for padding so we don't have to allocate all the time
// not synchronized because we don't care what the contents are
var pad [maxPad]byte

func (h *header) init(recType recType, reqId uint16, contentLength int) {
	h.Version = 1
	h.Type = recType
	h.Id = reqId
	h.ContentLength = uint16(contentLength)
	h.PaddingLength = uint8(-contentLength & 7)
}

// conn sends records over rwc
type conn struct {
	mutex    sync.Mutex
	rwc      io.ReadWriteCloser
	closeErr error
	closed   bool

	// to avoid allocations
	buf bytes.Buffer
	h   header
}

func newConn(rwc io.ReadWriteCloser) *conn {
	return &conn{rwc: rwc}
}

// Close closes the conn if it is not already closed.
func (c *conn) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if !c.closed {
		c.closeErr = c.rwc.Close()
		c.closed = true
	}
	return c.closeErr
}



// writeRecord writes and sends a single record.
func (c *conn) writeRecord(recType recType, reqId uint16, b []byte) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.buf.Reset()
	c.h.init(recType, reqId, len(b))
	if err := binary.Write(&c.buf, binary.BigEndian, c.h); err != nil {
		return err
	}
	if _, err := c.buf.Write(b); err != nil {
		return err
	}
	if _, err := c.buf.Write(pad[:c.h.PaddingLength]); err != nil {
		return err
	}
	_, err := c.rwc.Write(c.buf.Bytes())
	return err
}

func (c *conn) writeEndRequest(reqId uint16, appStatus int, protocolStatus uint8) error {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(appStatus))
	b[4] = protocolStatus
	return c.writeRecord(typeEndRequest, reqId, b)
}

func (c *conn) writePairs(recType recType, reqId uint16, pairs map[string]string) error {
	w := newWriter(c, recType, reqId)
	b := make([]byte, 8)
	for k, v := range pairs {
		n := encodeSize(b, uint32(len(k)))
		n += encodeSize(b[n:], uint32(len(v)))
		if _, err := w.Write(b[:n]); err != nil {
			return err
		}
		if _, err := w.WriteString(k); err != nil {
			return err
		}
		if _, err := w.WriteString(v); err != nil {
			return err
		}
	}
	w.Close()
	return nil
}



func encodeSize(b []byte, size uint32) int {
	if size > 127 {
		size |= 1 << 31
		binary.BigEndian.PutUint32(b, size)
		return 4
	}
	b[0] = byte(size)
	return 1
}

// bufWriter encapsulates bufio.Writer but also closes the underlying stream when
// Closed.
type bufWriter struct {
	closer io.Closer
	*bufio.Writer
}

func (w *bufWriter) Close() error {
	if err := w.Writer.Flush(); err != nil {
		w.closer.Close()
		return err
	}
	return w.closer.Close()
}

func newWriter(c *conn, recType recType, reqId uint16) *bufWriter {
	s := &streamWriter{c: c, recType: recType, reqId: reqId}
	w := bufio.NewWriterSize(s, maxWrite)
	return &bufWriter{s, w}
}

// streamWriter abstracts out the separation of a stream into discrete records.
// It only writes maxWrite bytes at a time.
type streamWriter struct {
	c       *conn
	recType recType
	reqId   uint16
}

func (w *streamWriter) Write(p []byte) (int, error) {
	nn := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxWrite {
			n = maxWrite
		}
		if err := w.c.writeRecord(w.recType, w.reqId, p[:n]); err != nil {
			return nn, err
		}
		nn += n
		p = p[n:]
	}
	return nn, nil
}

func (w *streamWriter) Close() error {
	// send empty record to close the stream
	return w.c.writeRecord(w.recType, w.reqId, nil)
}

