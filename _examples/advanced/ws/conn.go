package ws

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type (
	Socket interface {
		NetConn() net.Conn
		Request() *http.Request

		ReadText(timeout time.Duration) (body []byte, err error)
		WriteText(body []byte, timeout time.Duration) error
	}

	Conn interface {
		Socket() Socket
		ID() string
		String() string

		Write(msg Message) bool
		WriteAndWait(ctx context.Context, msg Message) Message

		Connect(ctx context.Context, namespace string) (NSConn, error)
		WaitConnect(ctx context.Context, namespace string) (NSConn, error)
		DisconnectFrom(ctx context.Context, namespace string) error
		DisconnectFromAll(ctx context.Context) error

		IsClient() bool
		Server() *Server

		Close()
		IsClosed() bool
	}

	NSConn interface {
		Conn() Conn

		Emit(event string, body []byte) bool
		Ask(ctx context.Context, event string, body []byte) Message

		JoinRoom(ctx context.Context, roomName string) (Room, error)
		Room(roomName string) Room

		// LeaveRoom(ctx context.Context, roomName string)
		Disconnect(ctx context.Context) error
	}

	Room interface {
		NSConn() NSConn

		Emit(event string, body []byte) bool
		Leave() error
	}
)

var (
	_ Conn   = (*conn)(nil)
	_ NSConn = (*nsConn)(nil)
	_ Room   = (*room)(nil)
)

type conn struct {
	// the ID generated by `Server#IDGenerator`.
	id string

	// the gorilla or gobwas socket.
	socket Socket

	// the defined namespaces, allowed to connect.
	namespaces Namespaces

	// the current connection's connected namespace.
	connectedNamespaces *connectedNamespaces

	// useful to terminate the broadcast waiter.
	closeCh chan struct{}
	// used to fire `conn#Close` once.
	once *uint32

	// messages that this connection is waiting for a reply.
	waitingMessagesMutex sync.RWMutex
	waitingMessages      map[string]chan Message // messages that this connection waits for a reply.

	// non-nil if server-side connection.
	server *Server

	// more than 0 if acknowledged.
	acknowledged *uint32

	// maximum wait time allowed to read a message from the connection.
	// Defaults to no timeout.
	readTimeout time.Duration
	// maximum wait time allowed to write a message to the connection.
	// Defaults to no timeout.
	writeTimeout time.Duration
}

func newConn(underline Socket, namespaces Namespaces) *conn {
	c := &conn{
		socket:     underline,
		namespaces: namespaces,
		connectedNamespaces: &connectedNamespaces{
			namespaces: make(map[string]*nsConn),
		},
		closeCh:         make(chan struct{}),
		once:            new(uint32),
		acknowledged:    new(uint32),
		waitingMessages: make(map[string]chan Message),
	}

	return c
}

func (c *conn) ID() string {
	return c.id
}

func (c *conn) String() string {
	return c.ID()
}

func (c *conn) Socket() Socket {
	return c.socket
}

func (c *conn) IsClient() bool {
	return c.server == nil
}

func (c *conn) Server() *Server {
	if c.IsClient() {
		return nil
	}

	return c.server
}

var (
	ackBinary   = []byte("ack")
	ackOKBinary = []byte("ack_ok")
)

func (c *conn) isAcknowledged() bool {
	return atomic.LoadUint32(c.acknowledged) > 0
}

func (c *conn) startReader() {
	if c.IsClosed() {
		return
	}
	defer c.Close()

	var (
		queue       = make([]*Message, 0)
		queueMutex  = new(sync.Mutex)
		handleQueue = func() {
			queueMutex.Lock()
			defer queueMutex.Unlock()

			for _, msg := range queue {
				c.handleMessage(*msg)
			}

			queue = nil
		}
	)

	for {
		b, err := c.socket.ReadText(c.readTimeout)
		if err != nil {
			return
		}

		if !c.isAcknowledged() && bytes.HasPrefix(b, ackBinary) {
			if c.IsClient() {
				id := string(b[len(ackBinary):])
				c.id = id
				atomic.StoreUint32(c.acknowledged, 1)
				c.socket.WriteText(ackOKBinary, c.writeTimeout)
				handleQueue()
			} else {
				if len(b) == len(ackBinary) {
					c.socket.WriteText(append(ackBinary, []byte(c.id)...), c.writeTimeout)
				} else {
					// its ackOK, answer from client when ID received and it's ready for write/read.
					atomic.StoreUint32(c.acknowledged, 1)
					handleQueue()
				}
			}

			continue
		}

		msg := deserializeMessage(nil, b)
		if msg.isInvalid {
			// fmt.Printf("%s[%d] is invalid payload\n", b, len(b))
			continue
		}

		if !c.isAcknowledged() {
			queueMutex.Lock()
			queue = append(queue, &msg)
			queueMutex.Unlock()

			continue
		}

		if !c.handleMessage(msg) {
			return
		}
	}
}

func (c *conn) handleMessage(msg Message) bool {
	if msg.wait != "" {
		c.waitingMessagesMutex.RLock()
		ch, ok := c.waitingMessages[msg.wait]
		c.waitingMessagesMutex.RUnlock()
		if ok {
			ch <- msg
			return true
		}
	}

	switch msg.Event {
	case OnNamespaceConnect:
		c.connectedNamespaces.replyConnect(c, msg)
	case OnNamespaceDisconnect:
		c.connectedNamespaces.replyDisconnect(c, msg)
	case OnRoomJoin:
		c.connectedNamespaces.get(msg.Namespace).replyRoomJoin(msg)
	case OnRoomLeave:
		c.connectedNamespaces.get(msg.Namespace).replyRoomLeave(msg)
	default:
		msg.IsLocal = false
		ns := c.connectedNamespaces.get(msg.Namespace)
		if ns != nil {
			err := ns.events.fireEvent(ns, msg)
			if err != nil {
				msg.Err = err
				c.Write(msg)
				if isManualCloseError(err) {
					return false // close the connection after sending the closing message.
				}
			}
		}
	}

	return true
}

func (c *conn) ask(ctx context.Context, msg Message) (Message, error) {
	if c.IsClosed() {
		return msg, CloseError{Code: -1, error: ErrWrite}
	}

	now := time.Now().UnixNano()
	msg.wait = strconv.FormatInt(now, 10)
	if c.IsClient() {
		msg.wait = "client_" + msg.wait
	}

	if ctx == nil {
		ctx = context.TODO()
	} else {
		if deadline, has := ctx.Deadline(); has {
			if deadline.Before(time.Now().Add(-1 * time.Second)) {
				return Message{}, context.DeadlineExceeded
			}
		}
	}

	ch := make(chan Message)
	c.waitingMessagesMutex.Lock()
	c.waitingMessages[msg.wait] = ch
	c.waitingMessagesMutex.Unlock()

	if !c.Write(msg) {
		return Message{}, ErrWrite
	}

	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case receive := <-ch:
		c.waitingMessagesMutex.Lock()
		delete(c.waitingMessages, receive.wait)
		c.waitingMessagesMutex.Unlock()

		return receive, receive.Err
	}
}

// DisconnectFrom gracefully disconnects from a namespace.
func (c *conn) DisconnectFrom(ctx context.Context, namespace string) error {
	return c.connectedNamespaces.askDisconnect(ctx, c, Message{
		Namespace: namespace,
		Event:     OnNamespaceDisconnect,
	}, true)
}

var ErrWrite = fmt.Errorf("write closed")

// const defaultWaitServerOrClientConnectTimeout = 8 * time.Second

// const waitConnectDuruation = 100 * time.Millisecond

// Nil context means try without timeout, wait until it connects to the specific namespace.
func (c *conn) WaitConnect(ctx context.Context, namespace string) (ns NSConn, err error) {
	if ctx == nil {
		ctx = context.TODO()
	}

	// if _, hasDeadline := ctx.Deadline(); !hasDeadline {
	// 	var cancel context.CancelFunc
	// 	ctx, cancel = context.WithTimeout(ctx, defaultWaitServerOrClientConnectTimeout)
	// 	defer cancel()
	// }

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			if ns == nil {
				ns = c.connectedNamespaces.get(namespace)
			}

			if ns != nil && c.isAcknowledged() {
				return
			}

			time.Sleep(syncWaitDur)
		}
	}

	return nil, ErrBadNamespace
}

const syncWaitDur = 15 * time.Millisecond

func (c *conn) Connect(ctx context.Context, namespace string) (NSConn, error) {
	if !c.IsClient() {
		for !c.isAcknowledged() {
			time.Sleep(syncWaitDur)
		}
	}

	return c.connectedNamespaces.askConnect(ctx, c, namespace)
}

// DisconnectFromAll gracefully disconnects from all namespaces.
func (c *conn) DisconnectFromAll(ctx context.Context) error {
	return c.connectedNamespaces.disconnectAll(ctx, c)
}

func (c *conn) IsClosed() bool {
	return atomic.LoadUint32(c.once) > 0
}

func (c *conn) Close() {
	if atomic.CompareAndSwapUint32(c.once, 0, 1) {
		close(c.closeCh)
		// fire the namespaces' disconnect event for both server and client.
		c.connectedNamespaces.forceDisconnectAll()

		c.waitingMessagesMutex.Lock()
		for wait := range c.waitingMessages {
			delete(c.waitingMessages, wait)
		}
		c.waitingMessagesMutex.Unlock()

		atomic.StoreUint32(c.acknowledged, 0)

		go func() {
			if !c.IsClient() {
				c.server.disconnect <- c
			}
		}()

		c.socket.NetConn().Close()
	}
}

func (c *conn) WriteAndWait(ctx context.Context, msg Message) Message {
	response, err := c.ask(ctx, msg)

	if !response.isError && err != nil {
		return Message{Err: err, isError: true}
	}

	return response
}

func (c *conn) Write(msg Message) bool {
	if c.IsClosed() {
		return false
	}

	// msg.from = c.ID()

	if !msg.isConnect() && !msg.isDisconnect() {
		ns := c.connectedNamespaces.get(msg.Namespace)
		if ns == nil {
			return false
		}

		if msg.Room != "" && !msg.isRoomJoin() && !msg.isRoomLeft() {
			ns.roomsMu.RLock()
			_, ok := ns.rooms[msg.Room]
			ns.roomsMu.RUnlock()
			if !ok {
				// tried to send to a not joined room.
				return false
			}
		}
	}

	err := c.socket.WriteText(serializeMessage(nil, msg), c.writeTimeout)
	if err != nil {
		if IsCloseError(err) {
			c.Close()
		}
		return false
	}

	return true
}
