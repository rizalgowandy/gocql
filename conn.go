// Copyright (c) 2012 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocql

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const defaultFrameSize = 4096

type Cluster interface {
	//HandleAuth(addr, method string) ([]byte, Challenger, error)
	HandleError(conn *Conn, err error, closed bool)
	HandleKeyspace(conn *Conn, keyspace string)
}

/* type Challenger interface {
	Challenge(data []byte) ([]byte, error)
} */

type ConnConfig struct {
	ProtoVersion int
	CQLVersion   string
	Keyspace     string
	Timeout      time.Duration
	NumStreams   int
}

// Conn is a single connection to a Cassandra node. It can be used to execute
// queries, but users are usually advised to use a more reliable, higher
// level API.
type Conn struct {
	conn    net.Conn
	timeout time.Duration

	uniq  chan uint8
	calls []callReq
	nwait int32

	prepMu sync.Mutex
	prep   map[string]*queryInfo

	cluster  Cluster
	addr     string
	keyspace string
}

// Connect establishes a connection to a Cassandra node.
// You must also call the Serve method before you can execute any queries.
func Connect(addr string, cfg ConnConfig, cluster Cluster) (*Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if cfg.NumStreams <= 0 || cfg.NumStreams > 128 {
		cfg.NumStreams = 128
	}
	c := &Conn{
		conn:    conn,
		uniq:    make(chan uint8, cfg.NumStreams),
		calls:   make([]callReq, cfg.NumStreams),
		prep:    make(map[string]*queryInfo),
		timeout: cfg.Timeout,
		addr:    conn.RemoteAddr().String(),
		cluster: cluster,
	}
	for i := 0; i < cap(c.uniq); i++ {
		c.uniq <- uint8(i)
	}

	if err := c.startup(&cfg); err != nil {
		return nil, err
	}

	go c.serve()

	return c, nil
}

func (c *Conn) startup(cfg *ConnConfig) error {
	req := make(frame, headerSize, defaultFrameSize)
	req.setHeader(protoRequest, 0, 0, opStartup)
	req.writeStringMap(map[string]string{
		"CQL_VERSION": cfg.CQLVersion,
	})
	resp, err := c.callSimple(req)
	if err != nil {
		return err
	} else if resp[3] == opError {
		return resp.readErrorFrame()
	} else if resp[3] != opReady {
		return ErrProtocol
	}

	return nil
}

// Serve starts the stream multiplexer for this connection, which is required
// to execute any queries. This method runs as long as the connection is
// open and is therefore usually called in a separate goroutine.
func (c *Conn) serve() {
	var err error
	for {
		var frame frame
		frame, err = c.recv()
		if err != nil {
			break
		}
		c.dispatch(frame)
	}

	c.conn.Close()
	for id := 0; id < len(c.calls); id++ {
		req := &c.calls[id]
		if atomic.LoadInt32(&req.active) == 1 {
			req.resp <- callResp{nil, err}
		}
	}
	c.cluster.HandleError(c, err, true)
}

func (c *Conn) recv() (frame, error) {
	resp := make(frame, headerSize, headerSize+512)
	c.conn.SetReadDeadline(time.Now().Add(c.timeout))
	n, last, pinged := 0, 0, false
	for n < len(resp) {
		nn, err := c.conn.Read(resp[n:])
		n += nn
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Timeout() {
				if n > last {
					// we hit the deadline but we made progress.
					// simply extend the deadline
					c.conn.SetReadDeadline(time.Now().Add(c.timeout))
					last = n
				} else if n == 0 && !pinged {
					c.conn.SetReadDeadline(time.Now().Add(c.timeout))
					if atomic.LoadInt32(&c.nwait) > 0 {
						go c.ping()
						pinged = true
					}
				} else {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
		if n == headerSize && len(resp) == headerSize {
			if resp[0] != protoResponse {
				return nil, ErrProtocol
			}
			resp.grow(resp.Length())
		}
	}
	return resp, nil
}

func (c *Conn) callSimple(req frame) (frame, error) {
	req.setLength(len(req) - headerSize)
	if _, err := c.conn.Write(req); err != nil {
		c.conn.Close()
		return nil, err
	}
	return c.recv()
}

func (c *Conn) call(req frame) (frame, error) {
	id := <-c.uniq
	req[2] = id

	call := &c.calls[id]
	call.resp = make(chan callResp, 1)
	atomic.AddInt32(&c.nwait, 1)
	atomic.StoreInt32(&call.active, 1)

	req.setLength(len(req) - headerSize)
	if _, err := c.conn.Write(req); err != nil {
		c.conn.Close()
		return nil, err
	}

	reply := <-call.resp
	call.resp = nil

	c.uniq <- id
	return reply.buf, reply.err
}

func (c *Conn) dispatch(resp frame) {
	id := int(resp[2])
	if id >= len(c.calls) {
		return
	}
	call := &c.calls[id]
	if !atomic.CompareAndSwapInt32(&call.active, 1, 0) {
		return
	}
	atomic.AddInt32(&c.nwait, -1)
	call.resp <- callResp{resp, nil}
}

func (c *Conn) ping() error {
	req := make(frame, headerSize)
	req.setHeader(protoRequest, 0, 0, opOptions)
	_, err := c.call(req)
	return err
}

func (c *Conn) prepareStatement(stmt string) (*queryInfo, error) {
	c.prepMu.Lock()
	info := c.prep[stmt]
	if info != nil {
		c.prepMu.Unlock()
		info.wg.Wait()
		return info, nil
	}
	info = new(queryInfo)
	info.wg.Add(1)
	c.prep[stmt] = info
	c.prepMu.Unlock()

	frame := make(frame, headerSize, defaultFrameSize)
	frame.setHeader(protoRequest, 0, 0, opPrepare)
	frame.writeLongString(stmt)
	frame.setLength(len(frame) - headerSize)

	frame, err := c.call(frame)
	if err != nil {
		return nil, err
	}
	if frame[3] == opError {
		return nil, frame.readErrorFrame()
	}
	frame.skipHeader()
	frame.readInt() // kind
	info.id = frame.readShortBytes()
	info.args = frame.readMetaData()
	info.rval = frame.readMetaData()
	info.wg.Done()
	return info, nil
}

func (c *Conn) switchKeyspace(keyspace string) error {
	if keyspace == "" || c.keyspace == keyspace {
		return nil
	}
	if _, err := c.ExecuteQuery(&Query{Stmt: "USE " + keyspace}); err != nil {
		return err
	}
	return nil
}

func (c *Conn) ExecuteQuery(qry *Query) (*Iter, error) {
	frame, err := c.executeQuery(qry)
	if err != nil {
		return nil, err
	}
	if frame[3] == opError {
		return nil, frame.readErrorFrame()
	} else if frame[3] == opResult {
		iter := new(Iter)
		iter.readFrame(frame)
		return iter, nil
	}
	return nil, nil
}

func (c *Conn) ExecuteBatch(batch *Batch) error {
	frame := make(frame, headerSize, defaultFrameSize)
	frame.setHeader(protoRequest, 0, 0, opBatch)
	frame.writeByte(byte(batch.Type))
	frame.writeShort(uint16(len(batch.Entries)))
	for i := 0; i < len(batch.Entries); i++ {
		entry := &batch.Entries[i]
		var info *queryInfo
		if len(entry.Args) > 0 {
			info, err := c.prepareStatement(entry.Stmt)
			if err != nil {
				return err
			}
			frame.writeByte(1)
			frame.writeShortBytes(info.id)
		} else {
			frame.writeByte(0)
			frame.writeLongString(entry.Stmt)
		}
		frame.writeShort(uint16(len(entry.Args)))
		for j := 0; j < len(entry.Args); j++ {
			val, err := Marshal(info.args[j].TypeInfo, entry.Args[i])
			if err != nil {
				return err
			}
			frame.writeBytes(val)
		}
	}
	frame.writeConsistency(batch.Cons)

	frame, err := c.call(frame)
	if err != nil {
		return err
	}

	if frame[3] == opError {
		return frame.readErrorFrame()
	}

	return nil
}

func (c *Conn) Close() {
	c.conn.Close()
}

func (c *Conn) Address() string {
	return c.addr
}

func (c *Conn) executeQuery(query *Query) (frame, error) {
	var info *queryInfo
	if len(query.Args) > 0 {
		var err error
		info, err = c.prepareStatement(query.Stmt)
		if err != nil {
			return nil, err
		}
	}

	frame := make(frame, headerSize, defaultFrameSize)
	if info == nil {
		frame.setHeader(protoRequest, 0, 0, opQuery)
		frame.writeLongString(query.Stmt)
	} else {
		frame.setHeader(protoRequest, 0, 0, opExecute)
		frame.writeShortBytes(info.id)
	}
	frame.writeConsistency(query.Cons)
	flags := uint8(0)
	if len(query.Args) > 0 {
		flags |= flagQueryValues
	}
	frame.writeByte(flags)
	if len(query.Args) > 0 {
		frame.writeShort(uint16(len(query.Args)))
		for i := 0; i < len(query.Args); i++ {
			val, err := Marshal(info.args[i].TypeInfo, query.Args[i])
			if err != nil {
				return nil, err
			}
			frame.writeBytes(val)
		}
	}

	frame, err := c.call(frame)
	if err != nil {
		return nil, err
	}

	if frame[3] == opResult {
		f := frame
		f.skipHeader()
		if f.readInt() == resultKindKeyspace {
			keyspace := f.readString()
			c.cluster.HandleKeyspace(c, keyspace)
		}
	}

	if frame[3] == opError {
		frame.skipHeader()
		code := frame.readInt()
		desc := frame.readString()
		return nil, Error{code, desc}
	}
	return frame, nil
}

func (c *Conn) UseKeyspace(keyspace string) error {
	frame := make(frame, headerSize, defaultFrameSize)
	frame.setHeader(protoRequest, 0, 0, opQuery)
	frame.writeLongString("USE " + keyspace)
	frame.writeConsistency(1)
	frame.writeByte(0)

	frame, err := c.call(frame)
	if err != nil {
		return err
	}

	if frame[3] == opError {
		frame.skipHeader()
		code := frame.readInt()
		desc := frame.readString()
		return Error{code, desc}
	}
	return nil
}

type queryInfo struct {
	id   []byte
	args []ColumnInfo
	rval []ColumnInfo
	wg   sync.WaitGroup
}

type callReq struct {
	active int32
	resp   chan callResp
}

type callResp struct {
	buf frame
	err error
}