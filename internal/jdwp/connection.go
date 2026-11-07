package jdwp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
)

const handshake = "JDWP-Handshake"

const (
	EventSingleStep             byte = 1
	EventBreakpoint             byte = 2
	EventException              byte = 4
	EventClassPrepare           byte = 8
	EventAccessWatchpoint       byte = 20
	EventModificationWatchpoint byte = 21
	EventVMStart                byte = 90
	EventVMDeath                byte = 99
	SuspendNone                 byte = 0
	SuspendEventThread          byte = 1
	SuspendAll                  byte = 2
)

// Connection multiplexes JDWP replies and asynchronous VM command packets.
// JDWP events are sent by the VM independently of requests; a request-only
// reader can deadlock as soon as a breakpoint or step event arrives first.
type Connection struct {
	conn       net.Conn
	writeMu    sync.Mutex
	mu         sync.Mutex
	nextID     uint32
	pending    map[uint32]chan commandResult
	terminal   error
	done       chan struct{}
	events     chan Event
	eventBatch uint64
	closeOnce  sync.Once
	objectID   int
	threadID   int
	frameID    int
	fieldID    int
	methodID   int
	classID    int
}

type commandResult struct {
	data []byte
	err  error
}

type Thread struct {
	ID   uint64
	Name string
}

type Frame struct {
	ID       uint64
	Location Location
}

type Location struct {
	TypeTag  byte
	ClassID  uint64
	MethodID uint64
	Index    int64
}

// Event is a decoded JDWP composite event. Fields are populated for the event
// kinds needed by DAP: breakpoint, step, exception, class prepare, VM start,
// and VM death.
type Event struct {
	Kind       byte
	RequestID  uint32
	ThreadID   uint64
	Location   Location
	TypeTag    byte
	TypeID     uint64
	Signature  string
	Status     uint32
	Exception  uint64
	ObjectID   uint64
	FieldID    uint64
	Catch      Location
	Suspend    byte
	BatchID    uint64
	BatchIndex int
	BatchCount int
	BatchStops bool
}

type ReferenceType struct {
	Tag       byte
	ID        uint64
	Signature string
	Status    uint32
}

type Method struct {
	ID        uint64
	Name      string
	Signature string
	Generic   string
	Modifiers uint32
}

type LineEntry struct {
	CodeIndex int64
	Line      int
}

type Variable struct {
	CodeIndex int64
	Name      string
	Signature string
	Generic   string
	Length    int
	Slot      int
}

type Field struct {
	ID        uint64
	Name      string
	Signature string
	Modifiers uint32
}

type Value struct {
	Tag      byte
	Integer  int64
	Float    float64
	ObjectID uint64
}

func Dial(address string) (*Connection, error) {
	c, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	if _, err = c.Write([]byte(handshake)); err != nil {
		_ = c.Close()
		return nil, err
	}
	answer := make([]byte, len(handshake))
	if _, err = io.ReadFull(c, answer); err != nil {
		_ = c.Close()
		return nil, err
	}
	if string(answer) != handshake {
		_ = c.Close()
		return nil, errors.New("invalid JDWP handshake")
	}
	conn := &Connection{
		conn: c, nextID: 1, pending: map[uint32]chan commandResult{},
		done: make(chan struct{}), events: make(chan Event, 4096),
	}
	go conn.readLoop()
	data, err := conn.command(1, 7, nil) // VirtualMachine.IDSizes
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if len(data) < 20 {
		_ = conn.Close()
		return nil, errors.New("short JDWP IDSizes response")
	}
	conn.fieldID = int(binary.BigEndian.Uint32(data[0:4]))
	conn.methodID = int(binary.BigEndian.Uint32(data[4:8]))
	conn.objectID = int(binary.BigEndian.Uint32(data[8:12]))
	conn.classID = int(binary.BigEndian.Uint32(data[12:16]))
	conn.frameID = int(binary.BigEndian.Uint32(data[16:20]))
	conn.threadID = conn.objectID
	return conn, nil
}

func (c *Connection) Events() <-chan Event { return c.events }

func (c *Connection) Close() error {
	c.fail(net.ErrClosed)
	return c.conn.Close()
}

func (c *Connection) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.terminal = err
		close(c.done)
		for id, pending := range c.pending {
			delete(c.pending, id)
			pending <- commandResult{err: err}
		}
		c.mu.Unlock()
		_ = c.conn.Close()
	})
}

func (c *Connection) readLoop() {
	defer close(c.events)
	for {
		header := make([]byte, 11)
		if _, err := io.ReadFull(c.conn, header); err != nil {
			c.fail(err)
			return
		}
		length := int(binary.BigEndian.Uint32(header[:4]))
		if length < 11 || length > 64*1024*1024 {
			c.fail(fmt.Errorf("invalid JDWP packet length %d", length))
			return
		}
		body := make([]byte, length-11)
		if _, err := io.ReadFull(c.conn, body); err != nil {
			c.fail(err)
			return
		}
		id := binary.BigEndian.Uint32(header[4:8])
		if header[8]&0x80 != 0 {
			code := binary.BigEndian.Uint16(header[9:11])
			result := commandResult{data: body}
			if code != 0 {
				result.err = fmt.Errorf("JDWP error %d", code)
			}
			c.mu.Lock()
			waiter := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if waiter == nil {
				c.fail(fmt.Errorf("JDWP response for unknown packet id %d", id))
				return
			}
			waiter <- result
			continue
		}
		if header[8] != 0 || header[9] != 64 || header[10] != 100 {
			c.fail(fmt.Errorf("unsupported JDWP command packet flags=%#x set=%d command=%d", header[8], header[9], header[10]))
			return
		}
		events, err := c.decodeCompositeEvents(body)
		if err != nil {
			c.fail(err)
			return
		}
		c.eventBatch++
		batchStops := false
		for _, event := range events {
			batchStops = batchStops || event.Kind == EventBreakpoint || event.Kind == EventSingleStep || event.Kind == EventException || event.Kind == EventAccessWatchpoint || event.Kind == EventModificationWatchpoint
		}
		for index, event := range events {
			event.BatchID = c.eventBatch
			event.BatchIndex = index
			event.BatchCount = len(events)
			event.BatchStops = batchStops
			select {
			case c.events <- event:
			case <-c.done:
				return
			}
		}
	}
}

func (c *Connection) decodeCompositeEvents(data []byte) ([]Event, error) {
	if len(data) < 5 {
		return nil, errors.New("short JDWP composite event packet")
	}
	suspend := data[0]
	count := int(binary.BigEndian.Uint32(data[1:5]))
	if count < 0 || count > 100000 {
		return nil, fmt.Errorf("invalid JDWP composite event count %d", count)
	}
	off := 5
	out := make([]Event, 0, count)
	for range count {
		if off+5 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		event := Event{Kind: data[off], RequestID: binary.BigEndian.Uint32(data[off+1 : off+5]), Suspend: suspend}
		off += 5
		var err error
		switch event.Kind {
		case EventSingleStep, EventBreakpoint:
			event.ThreadID, off, err = readID(data, off, c.threadID)
			if err == nil {
				event.Location, off, err = readLocation(data, off, c.classID, c.methodID)
			}
		case EventException:
			event.ThreadID, off, err = readID(data, off, c.threadID)
			if err == nil {
				event.Location, off, err = readLocation(data, off, c.classID, c.methodID)
			}
			if err == nil {
				event.Exception, off, err = readID(data, off, c.objectID)
			}
			if err == nil {
				event.Catch, off, err = readLocation(data, off, c.classID, c.methodID)
			}
		case EventAccessWatchpoint, EventModificationWatchpoint:
			event.ThreadID, off, err = readID(data, off, c.threadID)
			if err == nil {
				event.Location, off, err = readLocation(data, off, c.classID, c.methodID)
			}
			if err == nil && off < len(data) {
				var object Value
				object, off, err = c.readValue(data, off)
				event.ObjectID = object.ObjectID
			} else if err == nil {
				err = io.ErrUnexpectedEOF
			}
			if err == nil {
				event.FieldID, off, err = readID(data, off, c.fieldID)
			}
			if err == nil && event.Kind == EventModificationWatchpoint {
				_, off, err = c.readValue(data, off)
			}
		case EventClassPrepare:
			event.ThreadID, off, err = readID(data, off, c.threadID)
			if err == nil && off < len(data) {
				event.TypeTag = data[off]
				off++
			} else if err == nil {
				err = io.ErrUnexpectedEOF
			}
			if err == nil {
				event.TypeID, off, err = readID(data, off, c.classID)
			}
			if err == nil {
				event.Signature, off, err = readString(data, off)
			}
			if err == nil && off+4 <= len(data) {
				event.Status = binary.BigEndian.Uint32(data[off : off+4])
				off += 4
			} else if err == nil {
				err = io.ErrUnexpectedEOF
			}
		case EventVMStart:
			event.ThreadID, off, err = readID(data, off, c.threadID)
		case EventVMDeath:
			// The event carries no additional payload.
		default:
			return nil, fmt.Errorf("unsupported JDWP event kind %d", event.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("decode JDWP event kind %d: %w", event.Kind, err)
		}
		out = append(out, event)
	}
	if off != len(data) {
		return nil, fmt.Errorf("JDWP composite event has %d trailing bytes", len(data)-off)
	}
	return out, nil
}

func (c *Connection) AllThreads() ([]Thread, error) {
	data, err := c.command(1, 4, nil)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short AllThreads response")
	}
	count, off := int(binary.BigEndian.Uint32(data[:4])), 4
	if count < 0 || count > 100000 {
		return nil, fmt.Errorf("invalid thread count %d", count)
	}
	threads := make([]Thread, 0, count)
	for range count {
		id, next, err := readID(data, off, c.threadID)
		if err != nil {
			return nil, err
		}
		off = next
		nameBytes, err := c.command(11, 1, encodeID(id, c.threadID))
		if err != nil {
			continue
		}
		name, _, err := readString(nameBytes, 0)
		if err == nil {
			threads = append(threads, Thread{ID: id, Name: name})
		}
	}
	return threads, nil
}

func (c *Connection) Frames(thread uint64) ([]Frame, error) {
	payload := append(encodeID(thread, c.threadID), make([]byte, 8)...)
	data, err := c.command(11, 6, payload)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short Frames response")
	}
	count, off := int(binary.BigEndian.Uint32(data[:4])), 4
	if count < 0 || count > 100000 {
		return nil, fmt.Errorf("invalid frame count %d", count)
	}
	frames := make([]Frame, 0, count)
	for range count {
		id, next, err := readID(data, off, c.frameID)
		if err != nil {
			return nil, err
		}
		off = next
		location, next, err := readLocation(data, off, c.classID, c.methodID)
		if err != nil {
			return nil, err
		}
		off = next
		frames = append(frames, Frame{ID: id, Location: location})
	}
	return frames, nil
}

func (c *Connection) ClassesBySignature(signature string) ([]ReferenceType, error) {
	data, err := c.command(1, 2, encodeString(signature))
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short ClassesBySignature response")
	}
	count, off := int(binary.BigEndian.Uint32(data[:4])), 4
	if count > 100000 {
		return nil, fmt.Errorf("invalid matching class count %d", count)
	}
	out := make([]ReferenceType, 0, count)
	for range count {
		if off >= len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		class := ReferenceType{Tag: data[off]}
		off++
		class.ID, off, err = readID(data, off, c.classID)
		if err != nil || off+4 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		class.Status = binary.BigEndian.Uint32(data[off : off+4])
		off += 4
		class.Signature = signature
		out = append(out, class)
	}
	return out, nil
}

func (c *Connection) AllClasses() ([]ReferenceType, error) {
	data, err := c.command(1, 3, nil)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short AllClasses response")
	}
	count, off := int(binary.BigEndian.Uint32(data[:4])), 4
	if count > 1000000 {
		return nil, fmt.Errorf("invalid class count %d", count)
	}
	out := make([]ReferenceType, 0, count)
	for range count {
		if off >= len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		class := ReferenceType{Tag: data[off]}
		off++
		class.ID, off, err = readID(data, off, c.classID)
		if err != nil {
			return nil, err
		}
		class.Signature, off, err = readString(data, off)
		if err != nil || off+4 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		class.Status = binary.BigEndian.Uint32(data[off : off+4])
		off += 4
		out = append(out, class)
	}
	return out, nil
}

func (c *Connection) ClassSignature(classID uint64) (string, error) {
	data, err := c.command(2, 1, encodeID(classID, c.classID))
	if err != nil {
		return "", err
	}
	value, _, err := readString(data, 0)
	return value, err
}

func (c *Connection) SourceFile(classID uint64) (string, error) {
	data, err := c.command(2, 7, encodeID(classID, c.classID))
	if err != nil {
		return "", err
	}
	value, _, err := readString(data, 0)
	return value, err
}

func (c *Connection) Methods(classID uint64) ([]Method, error) {
	data, err := c.command(2, 5, encodeID(classID, c.classID))
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short Methods response")
	}
	count, off := int(binary.BigEndian.Uint32(data[:4])), 4
	if count > 100000 {
		return nil, fmt.Errorf("invalid method count %d", count)
	}
	out := make([]Method, 0, count)
	for range count {
		method := Method{}
		method.ID, off, err = readID(data, off, c.methodID)
		if err != nil {
			return nil, err
		}
		method.Name, off, err = readString(data, off)
		if err != nil {
			return nil, err
		}
		method.Signature, off, err = readString(data, off)
		if err != nil {
			return nil, err
		}
		method.Generic, off, err = readString(data, off)
		if err != nil || off+4 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		method.Modifiers = binary.BigEndian.Uint32(data[off : off+4])
		off += 4
		out = append(out, method)
	}
	return out, nil
}

func (c *Connection) LineTable(classID, methodID uint64) ([]LineEntry, error) {
	payload := append(encodeID(classID, c.classID), encodeID(methodID, c.methodID)...)
	data, err := c.command(6, 1, payload)
	if err != nil {
		return nil, err
	}
	if len(data) < 20 {
		return nil, errors.New("short LineTable response")
	}
	count := int(binary.BigEndian.Uint32(data[16:20]))
	off := 20
	if count > 1000000 || off+count*12 > len(data) {
		return nil, fmt.Errorf("invalid line table count %d", count)
	}
	out := make([]LineEntry, 0, count)
	for range count {
		index := int64(binary.BigEndian.Uint64(data[off : off+8]))
		line := int(int32(binary.BigEndian.Uint32(data[off+8 : off+12])))
		out = append(out, LineEntry{CodeIndex: index, Line: line})
		off += 12
	}
	return out, nil
}

func (c *Connection) Variables(classID, methodID uint64) ([]Variable, error) {
	payload := append(encodeID(classID, c.classID), encodeID(methodID, c.methodID)...)
	data, err := c.command(6, variableTableCommand, payload)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, errors.New("short VariableTable response")
	}
	count := int(binary.BigEndian.Uint32(data[4:8]))
	off := 8
	if count > 100000 || off+count*4 > len(data) {
		return nil, fmt.Errorf("invalid local variable count %d", count)
	}
	out := make([]Variable, 0, count)
	for range count {
		variable := Variable{}
		variable.CodeIndex = int64(binary.BigEndian.Uint64(data[off : off+8]))
		variable.Length = int(binary.BigEndian.Uint32(data[off+8 : off+12]))
		variable.Name, off, err = readString(data, off+12)
		if err != nil {
			return nil, err
		}
		variable.Signature, off, err = readString(data, off)
		if err != nil {
			return nil, err
		}
		variable.Generic, off, err = readString(data, off)
		if err != nil || off+4 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		variable.Slot = int(binary.BigEndian.Uint32(data[off : off+4]))
		off += 4
		out = append(out, variable)
	}
	return out, nil
}

func (c *Connection) FrameValues(threadID, frameID uint64, variables []Variable) ([]Value, error) {
	payload := append(encodeID(threadID, c.threadID), encodeID(frameID, c.frameID)...)
	payload = append(payload, encodeUint32(uint32(len(variables)))...)
	for _, variable := range variables {
		payload = append(payload, encodeUint32(uint32(variable.Slot))...)
		payload = append(payload, signatureTag(variable.Signature))
	}
	data, err := c.command(16, 1, payload)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || int(binary.BigEndian.Uint32(data[:4])) != len(variables) {
		return nil, errors.New("invalid StackFrame.GetValues response")
	}
	off := 4
	out := make([]Value, 0, len(variables))
	for range variables {
		value, next, err := c.readValue(data, off)
		if err != nil {
			return nil, err
		}
		off = next
		out = append(out, value)
	}
	return out, nil
}

func (c *Connection) SetFrameValue(threadID, frameID uint64, variable Variable, value Value) error {
	payload := append(encodeID(threadID, c.threadID), encodeID(frameID, c.frameID)...)
	payload = append(payload, encodeUint32(1)...)
	payload = append(payload, encodeUint32(uint32(variable.Slot))...)
	payload = append(payload, encodeUntypedValue(value, variable.Signature, c.objectID)...)
	_, err := c.command(16, 2, payload)
	return err
}

func (c *Connection) Fields(classID uint64) ([]Field, error) {
	data, err := c.command(2, 4, encodeID(classID, c.classID))
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, errors.New("short Fields response")
	}
	count, off := int(binary.BigEndian.Uint32(data[:4])), 4
	if count > 100000 {
		return nil, fmt.Errorf("invalid field count %d", count)
	}
	out := make([]Field, 0, count)
	for range count {
		field := Field{}
		field.ID, off, err = readID(data, off, c.fieldID)
		if err != nil {
			return nil, err
		}
		field.Name, off, err = readString(data, off)
		if err != nil {
			return nil, err
		}
		field.Signature, off, err = readString(data, off)
		if err != nil {
			return nil, err
		}
		_, off, err = readString(data, off) // generic signature
		if err != nil || off+4 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		field.Modifiers = binary.BigEndian.Uint32(data[off : off+4])
		off += 4
		out = append(out, field)
	}
	return out, nil
}

func (c *Connection) ObjectType(objectID uint64) (byte, uint64, error) {
	data, err := c.command(9, 1, encodeID(objectID, c.objectID))
	if err != nil {
		return 0, 0, err
	}
	if len(data) < 1 {
		return 0, 0, errors.New("short ObjectReference.ReferenceType response")
	}
	typeID, _, err := readID(data, 1, c.classID)
	return data[0], typeID, err
}

func (c *Connection) ObjectValues(objectID uint64, fields []Field) ([]Value, error) {
	payload := append(encodeID(objectID, c.objectID), encodeUint32(uint32(len(fields)))...)
	for _, field := range fields {
		payload = append(payload, encodeID(field.ID, c.fieldID)...)
	}
	data, err := c.command(9, 2, payload)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || int(binary.BigEndian.Uint32(data[:4])) != len(fields) {
		return nil, errors.New("invalid ObjectReference.GetValues response")
	}
	off := 4
	out := make([]Value, 0, len(fields))
	for range fields {
		value, next, err := c.readValue(data, off)
		if err != nil {
			return nil, err
		}
		off = next
		out = append(out, value)
	}
	return out, nil
}

func (c *Connection) SetObjectValue(objectID uint64, field Field, value Value) error {
	payload := append(encodeID(objectID, c.objectID), encodeUint32(1)...)
	payload = append(payload, encodeID(field.ID, c.fieldID)...)
	payload = append(payload, encodeUntypedValue(value, field.Signature, c.objectID)...)
	_, err := c.command(9, 3, payload)
	return err
}

func (c *Connection) ArrayLength(arrayID uint64) (int, error) {
	data, err := c.command(13, 1, encodeID(arrayID, c.objectID))
	if err != nil {
		return 0, err
	}
	if len(data) < 4 {
		return 0, errors.New("short ArrayReference.GetLength response")
	}
	return int(int32(binary.BigEndian.Uint32(data[:4]))), nil
}

func (c *Connection) ArrayValues(arrayID uint64, first, count int) ([]Value, error) {
	payload := append(encodeID(arrayID, c.objectID), encodeUint32(uint32(first))...)
	payload = append(payload, encodeUint32(uint32(count))...)
	data, err := c.command(13, 2, payload)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || int(binary.BigEndian.Uint32(data[:4])) != count {
		return nil, errors.New("invalid ArrayReference.GetValues response")
	}
	off := 4
	out := make([]Value, 0, count)
	for range count {
		value, next, err := c.readValue(data, off)
		if err != nil {
			return nil, err
		}
		off = next
		out = append(out, value)
	}
	return out, nil
}

func (c *Connection) StringValue(stringID uint64) (string, error) {
	data, err := c.command(10, 1, encodeID(stringID, c.objectID))
	if err != nil {
		return "", err
	}
	value, _, err := readString(data, 0)
	return value, err
}

func (c *Connection) readValue(data []byte, off int) (Value, int, error) {
	if off >= len(data) {
		return Value{}, off, io.ErrUnexpectedEOF
	}
	value := Value{Tag: data[off]}
	off++
	width := 0
	switch value.Tag {
	case 'Z', 'B':
		width = 1
	case 'C', 'S':
		width = 2
	case 'F', 'I':
		width = 4
	case 'D', 'J':
		width = 8
	case 'L', '[', 's', 't', 'g', 'l', 'c':
		var err error
		value.ObjectID, off, err = readID(data, off, c.objectID)
		if err != nil {
			return Value{}, off, err
		}
		return value, off, nil
	case 'V':
		return value, off, nil
	default:
		return Value{}, off, fmt.Errorf("unknown JDWP value tag %q", value.Tag)
	}
	if off+width > len(data) {
		return Value{}, off, io.ErrUnexpectedEOF
	}
	switch value.Tag {
	case 'Z':
		value.Integer = int64(data[off])
	case 'B':
		value.Integer = int64(int8(data[off]))
	case 'C':
		value.Integer = int64(binary.BigEndian.Uint16(data[off : off+2]))
	case 'S':
		value.Integer = int64(int16(binary.BigEndian.Uint16(data[off : off+2])))
	case 'I':
		value.Integer = int64(int32(binary.BigEndian.Uint32(data[off : off+4])))
	case 'J':
		value.Integer = int64(binary.BigEndian.Uint64(data[off : off+8]))
	case 'F':
		value.Float = float64(math.Float32frombits(binary.BigEndian.Uint32(data[off : off+4])))
	case 'D':
		value.Float = math.Float64frombits(binary.BigEndian.Uint64(data[off : off+8]))
	}
	return value, off + width, nil
}

func signatureTag(signature string) byte {
	if signature == "" {
		return 'L'
	}
	if signature[0] == '[' {
		return '['
	}
	return signature[0]
}

func encodeUntypedValue(value Value, signature string, objectIDWidth int) []byte {
	if signature == "" {
		return encodeID(value.ObjectID, objectIDWidth)
	}
	switch signature[0] {
	case 'Z', 'B':
		return []byte{byte(value.Integer)}
	case 'C', 'S':
		data := make([]byte, 2)
		binary.BigEndian.PutUint16(data, uint16(value.Integer))
		return data
	case 'I':
		return encodeUint32(uint32(value.Integer))
	case 'F':
		data := make([]byte, 4)
		binary.BigEndian.PutUint32(data, math.Float32bits(float32(value.Float)))
		return data
	case 'J':
		return encodeInt64(value.Integer)
	case 'D':
		return encodeInt64(int64(math.Float64bits(value.Float)))
	case 'L', '[':
		return encodeID(value.ObjectID, objectIDWidth)
	default:
		return nil
	}
}

func writeFull(writer io.Writer, data []byte) (int, error) {
	written := 0
	for written < len(data) {
		n, err := writer.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

const variableTableCommand byte = 2

func (c *Connection) SetClassPrepare(pattern string) (uint32, error) {
	payload := []byte{EventClassPrepare, SuspendAll, 0, 0, 0, 1, 5}
	payload = append(payload, encodeString(pattern)...)
	return c.setEvent(payload)
}

func (c *Connection) SetBreakpoint(location Location) (uint32, error) {
	payload := []byte{EventBreakpoint, SuspendAll, 0, 0, 0, 1, 7, location.TypeTag}
	payload = append(payload, encodeID(location.ClassID, c.classID)...)
	payload = append(payload, encodeID(location.MethodID, c.methodID)...)
	payload = append(payload, encodeInt64(location.Index)...)
	return c.setEvent(payload)
}

func (c *Connection) SetFieldWatch(classTag byte, classID, fieldID uint64, access bool) (uint32, error) {
	kind := EventModificationWatchpoint
	if access {
		kind = EventAccessWatchpoint
	}
	payload := []byte{kind, SuspendAll, 0, 0, 0, 1, 9, classTag}
	payload = append(payload, encodeID(classID, c.classID)...)
	payload = append(payload, encodeID(fieldID, c.fieldID)...)
	return c.setEvent(payload)
}

func (c *Connection) SetStep(threadID uint64, depth byte) (uint32, error) {
	payload := []byte{EventSingleStep, SuspendAll, 0, 0, 0, 1, 10}
	payload = append(payload, encodeID(threadID, c.threadID)...)
	payload = append(payload, 0, 1, depth) // line granularity, INTO/OVER/OUT
	return c.setEvent(payload)
}

func (c *Connection) SetExceptionBreakpoints(caught, uncaught bool) (uint32, error) {
	payload := []byte{EventException, SuspendAll, 0, 0, 0, 1, 8}
	payload = append(payload, encodeID(0, c.objectID)...)
	flags := byte(0)
	if caught {
		flags |= 1
	}
	if uncaught {
		flags |= 2
	}
	payload = append(payload, flags)
	return c.setEvent(payload)
}

func (c *Connection) setEvent(payload []byte) (uint32, error) {
	data, err := c.command(15, 1, payload)
	if err != nil {
		return 0, err
	}
	if len(data) != 4 {
		return 0, errors.New("invalid EventRequest.Set response")
	}
	return binary.BigEndian.Uint32(data), nil
}

func (c *Connection) ClearEvent(kind byte, requestID uint32) error {
	payload := []byte{kind}
	payload = append(payload, encodeUint32(requestID)...)
	_, err := c.command(15, 2, payload)
	return err
}

func (c *Connection) Resume() error  { _, err := c.command(1, 9, nil); return err }
func (c *Connection) Suspend() error { _, err := c.command(1, 8, nil); return err }

func (c *Connection) command(set, command byte, payload []byte) ([]byte, error) {
	c.mu.Lock()
	if c.terminal != nil {
		err := c.terminal
		c.mu.Unlock()
		return nil, err
	}
	id := c.nextID
	c.nextID++
	waiter := make(chan commandResult, 1)
	c.pending[id] = waiter
	c.mu.Unlock()

	length := 11 + len(payload)
	packet := make([]byte, length)
	binary.BigEndian.PutUint32(packet[:4], uint32(length))
	binary.BigEndian.PutUint32(packet[4:8], id)
	packet[8], packet[9], packet[10] = 0, set, command
	copy(packet[11:], payload)
	c.writeMu.Lock()
	_, writeErr := writeFull(c.conn, packet)
	c.writeMu.Unlock()
	if writeErr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.fail(writeErr)
		return nil, writeErr
	}
	select {
	case result := <-waiter:
		return result.data, result.err
	case <-c.done:
		c.mu.Lock()
		err := c.terminal
		c.mu.Unlock()
		if err == nil {
			err = net.ErrClosed
		}
		return nil, err
	}
}

func encodeID(id uint64, width int) []byte {
	b := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		b[i] = byte(id)
		id >>= 8
	}
	return b
}

func encodeString(value string) []byte {
	data := make([]byte, 4+len(value))
	binary.BigEndian.PutUint32(data[:4], uint32(len(value)))
	copy(data[4:], value)
	return data
}

func encodeUint32(value uint32) []byte {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, value)
	return data
}

func encodeInt64(value int64) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(value))
	return data
}

func readID(b []byte, off, width int) (uint64, int, error) {
	if width < 1 || off+width > len(b) {
		return 0, off, io.ErrUnexpectedEOF
	}
	var id uint64
	for _, value := range b[off : off+width] {
		id = id<<8 | uint64(value)
	}
	return id, off + width, nil
}

func readString(b []byte, off int) (string, int, error) {
	if off+4 > len(b) {
		return "", off, io.ErrUnexpectedEOF
	}
	n := int(binary.BigEndian.Uint32(b[off : off+4]))
	off += 4
	if n < 0 || off+n > len(b) {
		return "", off, io.ErrUnexpectedEOF
	}
	return string(b[off : off+n]), off + n, nil
}

func readLocation(b []byte, off, classID, methodID int) (Location, int, error) {
	if off >= len(b) {
		return Location{}, off, io.ErrUnexpectedEOF
	}
	location := Location{TypeTag: b[off]}
	off++
	var err error
	location.ClassID, off, err = readID(b, off, classID)
	if err != nil {
		return Location{}, off, err
	}
	location.MethodID, off, err = readID(b, off, methodID)
	if err != nil {
		return Location{}, off, err
	}
	if off+8 > len(b) {
		return Location{}, off, io.ErrUnexpectedEOF
	}
	location.Index = int64(binary.BigEndian.Uint64(b[off : off+8]))
	return location, off + 8, nil
}
