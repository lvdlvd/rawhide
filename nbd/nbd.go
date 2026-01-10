// Package nbd implements an NBD (Network Block Device) server.
// It exposes an io.ReaderAt as a block device via the Linux NBD protocol.
package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
)

// NBD protocol constants
const (
	nbdMagic            = uint64(0x4e42444d41474943) // "NBDMAGIC"
	nbdOptionMagic      = uint64(0x49484156454F5054) // "IHAVEOPT"
	nbdReplyMagic       = uint64(0x3e889045565a9)
	nbdRequestMagic     = uint32(0x25609513)
	nbdReplyMagicSimple = uint32(0x67446698)

	nbdFlagFixedNewstyle  = uint16(1 << 0)
	nbdFlagNoZeroes       = uint16(1 << 1)
	nbdFlagCFixedNewstyle = uint32(1 << 0)
	nbdFlagCNoZeroes      = uint32(1 << 1)

	nbdFlagHasFlags  = uint16(1 << 0)
	nbdFlagReadOnly  = uint16(1 << 1)
	nbdFlagSendFlush = uint16(1 << 2)
	nbdFlagSendFUA   = uint16(1 << 3)
	nbdFlagSendTrim  = uint16(1 << 5)

	nbdOptExportName = uint32(1)
	nbdOptAbort      = uint32(2)
	nbdOptList       = uint32(3)
	nbdOptGo         = uint32(7)

	nbdRepAck        = uint32(1)
	nbdRepServer     = uint32(2)
	nbdRepInfo       = uint32(3)
	nbdRepErrUnsup   = uint32(0x80000001)
	nbdRepErrUnknown = uint32(0x80000006)

	nbdInfoExport    = uint16(0)
	nbdInfoBlockSize = uint16(3)

	nbdCmdRead  = uint16(0)
	nbdCmdWrite = uint16(1)
	nbdCmdDisc  = uint16(2)
	nbdCmdFlush = uint16(3)
	nbdCmdTrim  = uint16(4)

	nbdErrNone  = uint32(0)
	nbdErrPerm  = uint32(1)
	nbdErrIO    = uint32(5)
	nbdErrInval = uint32(22)

	defaultBlockSize = uint32(4096)
)

// Export defines a named block device to expose
type Export struct {
	Name     string       // Export name that clients use to connect
	Reader   io.ReaderAt  // Data source
	Writer   io.WriterAt  // Optional: data sink for writes (nil = read-only)
	Size     int64        // Size of the export in bytes
}

// Server represents the NBD server
type Server struct {
	socketPath string
	exports    map[string]*Export
	exportsMu  sync.RWMutex
	listener   net.Listener
	done       chan struct{}
	logger     *log.Logger
}

// session represents an active client connection
type session struct {
	server   *Server
	conn     net.Conn
	export   *Export
	noZeroes bool
}

// NewServer creates a new NBD server
func NewServer(socketPath string) *Server {
	return &Server{
		socketPath: socketPath,
		exports:    make(map[string]*Export),
		done:       make(chan struct{}),
		logger:     log.New(os.Stderr, "nbd: ", log.LstdFlags),
	}
}

// SetLogger sets a custom logger
func (s *Server) SetLogger(l *log.Logger) {
	s.logger = l
}

// AddExport registers a new export
func (s *Server) AddExport(exp *Export) error {
	s.exportsMu.Lock()
	defer s.exportsMu.Unlock()

	if _, exists := s.exports[exp.Name]; exists {
		return fmt.Errorf("export %q already exists", exp.Name)
	}

	s.exports[exp.Name] = exp
	return nil
}

// getExport retrieves an export by name
func (s *Server) getExport(name string) *Export {
	s.exportsMu.RLock()
	defer s.exportsMu.RUnlock()
	return s.exports[name]
}

// listExports returns all export names
func (s *Server) listExports() []string {
	s.exportsMu.RLock()
	defer s.exportsMu.RUnlock()

	names := make([]string, 0, len(s.exports))
	for name := range s.exports {
		names = append(names, name)
	}
	return names
}

// Serve starts the server and blocks until shutdown
func (s *Server) Serve() error {
	if len(s.exports) == 0 {
		return errors.New("no exports defined")
	}

	// Remove existing socket file if present
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	s.listener = listener

	// Make socket accessible
	if err := os.Chmod(s.socketPath, 0660); err != nil {
		s.logger.Printf("Warning: failed to chmod socket: %v", err)
	}

	s.logger.Printf("Listening on unix:%s", s.socketPath)
	for _, exp := range s.exports {
		roStr := ""
		if exp.Writer == nil {
			roStr = " (read-only)"
		}
		s.logger.Printf("Export %q: %d bytes%s", exp.Name, exp.Size, roStr)
	}
	s.logger.Printf("Connect with: sudo nbd-client -N <export-name> -unix %s /dev/nbdX", s.socketPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				s.logger.Printf("Accept error: %v", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

// Close shuts down the server
func (s *Server) Close() error {
	close(s.done)

	if s.listener != nil {
		s.listener.Close()
	}

	os.Remove(s.socketPath)
	return nil
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	s.logger.Printf("New connection from %s", conn.RemoteAddr())

	sess := &session{
		server: s,
		conn:   conn,
	}

	if err := sess.negotiate(); err != nil {
		s.logger.Printf("Negotiation failed: %v", err)
		return
	}

	if err := sess.transmit(); err != nil {
		if err != io.EOF {
			s.logger.Printf("Transmission error: %v", err)
		}
	}

	s.logger.Printf("Connection closed (export: %s)", sess.export.Name)
}

func (sess *session) negotiate() error {
	// Send server greeting
	greeting := make([]byte, 18)
	binary.BigEndian.PutUint64(greeting[0:8], nbdMagic)
	binary.BigEndian.PutUint64(greeting[8:16], nbdOptionMagic)
	binary.BigEndian.PutUint16(greeting[16:18], nbdFlagFixedNewstyle|nbdFlagNoZeroes)

	if _, err := sess.conn.Write(greeting); err != nil {
		return fmt.Errorf("failed to send greeting: %w", err)
	}

	// Read client flags
	clientFlags := make([]byte, 4)
	if _, err := io.ReadFull(sess.conn, clientFlags); err != nil {
		return fmt.Errorf("failed to read client flags: %w", err)
	}

	flags := binary.BigEndian.Uint32(clientFlags)
	sess.noZeroes = (flags & nbdFlagCNoZeroes) != 0

	// Option haggling
	for {
		optHeader := make([]byte, 16)
		if _, err := io.ReadFull(sess.conn, optHeader); err != nil {
			return fmt.Errorf("failed to read option header: %w", err)
		}

		magic := binary.BigEndian.Uint64(optHeader[0:8])
		if magic != nbdOptionMagic {
			return fmt.Errorf("bad option magic: %x", magic)
		}

		optType := binary.BigEndian.Uint32(optHeader[8:12])
		optLen := binary.BigEndian.Uint32(optHeader[12:16])

		optData := make([]byte, optLen)
		if optLen > 0 {
			if _, err := io.ReadFull(sess.conn, optData); err != nil {
				return fmt.Errorf("failed to read option data: %w", err)
			}
		}

		done, err := sess.handleOption(optType, optData)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (sess *session) handleOption(optType uint32, optData []byte) (done bool, err error) {
	switch optType {
	case nbdOptExportName:
		exportName := string(optData)
		export := sess.server.getExport(exportName)
		if export == nil {
			return false, fmt.Errorf("unknown export: %s", exportName)
		}
		sess.export = export
		return true, sess.sendOldstyleExportInfo()

	case nbdOptGo:
		exportName := ""
		if len(optData) >= 4 {
			nameLen := binary.BigEndian.Uint32(optData[0:4])
			if nameLen > 0 && int(4+nameLen) <= len(optData) {
				exportName = string(optData[4 : 4+nameLen])
			}
		}

		export := sess.server.getExport(exportName)
		if export == nil && exportName == "" {
			// Try first export as default
			exports := sess.server.listExports()
			if len(exports) > 0 {
				export = sess.server.getExport(exports[0])
			}
		}

		if export == nil {
			sess.sendOptionReply(optType, nbdRepErrUnknown, nil)
			return false, nil
		}

		sess.export = export
		if err := sess.sendExportInfo(optType); err != nil {
			return false, err
		}
		return true, nil

	case nbdOptList:
		for _, name := range sess.server.listExports() {
			nameData := make([]byte, 4+len(name))
			binary.BigEndian.PutUint32(nameData[0:4], uint32(len(name)))
			copy(nameData[4:], name)
			sess.sendOptionReply(optType, nbdRepServer, nameData)
		}
		sess.sendOptionReply(optType, nbdRepAck, nil)
		return false, nil

	case nbdOptAbort:
		sess.sendOptionReply(optType, nbdRepAck, nil)
		return false, errors.New("client aborted")

	default:
		sess.sendOptionReply(optType, nbdRepErrUnsup, nil)
		return false, nil
	}
}

func (sess *session) sendOptionReply(option, replyType uint32, data []byte) error {
	reply := make([]byte, 20+len(data))
	binary.BigEndian.PutUint64(reply[0:8], nbdReplyMagic)
	binary.BigEndian.PutUint32(reply[8:12], option)
	binary.BigEndian.PutUint32(reply[12:16], replyType)
	binary.BigEndian.PutUint32(reply[16:20], uint32(len(data)))
	if len(data) > 0 {
		copy(reply[20:], data)
	}
	_, err := sess.conn.Write(reply)
	return err
}

func (sess *session) sendExportInfo(option uint32) error {
	exp := sess.export

	// Send NBD_INFO_EXPORT
	infoExport := make([]byte, 12)
	binary.BigEndian.PutUint16(infoExport[0:2], nbdInfoExport)
	binary.BigEndian.PutUint64(infoExport[2:10], uint64(exp.Size))
	flags := nbdFlagHasFlags | nbdFlagSendFlush | nbdFlagSendFUA
	if exp.Writer == nil {
		flags |= nbdFlagReadOnly
	}
	binary.BigEndian.PutUint16(infoExport[10:12], flags)
	if err := sess.sendOptionReply(option, nbdRepInfo, infoExport); err != nil {
		return err
	}

	// Send NBD_INFO_BLOCK_SIZE
	blockInfo := make([]byte, 14)
	binary.BigEndian.PutUint16(blockInfo[0:2], nbdInfoBlockSize)
	binary.BigEndian.PutUint32(blockInfo[2:6], 1)
	binary.BigEndian.PutUint32(blockInfo[6:10], defaultBlockSize)
	binary.BigEndian.PutUint32(blockInfo[10:14], 32*1024*1024)
	if err := sess.sendOptionReply(option, nbdRepInfo, blockInfo); err != nil {
		return err
	}

	return sess.sendOptionReply(option, nbdRepAck, nil)
}

func (sess *session) sendOldstyleExportInfo() error {
	exp := sess.export
	respLen := 10
	if !sess.noZeroes {
		respLen = 134
	}

	resp := make([]byte, respLen)
	binary.BigEndian.PutUint64(resp[0:8], uint64(exp.Size))
	flags := nbdFlagHasFlags | nbdFlagSendFlush | nbdFlagSendFUA
	if exp.Writer == nil {
		flags |= nbdFlagReadOnly
	}
	binary.BigEndian.PutUint16(resp[8:10], flags)

	_, err := sess.conn.Write(resp)
	return err
}

func (sess *session) transmit() error {
	header := make([]byte, 28)
	exp := sess.export

	sess.server.logger.Printf("Transmission phase for export %q (%d bytes)", exp.Name, exp.Size)

	for {
		if _, err := io.ReadFull(sess.conn, header); err != nil {
			return err
		}

		magic := binary.BigEndian.Uint32(header[0:4])
		if magic != nbdRequestMagic {
			return fmt.Errorf("bad request magic: %x", magic)
		}

		cmdType := binary.BigEndian.Uint16(header[6:8])
		handle := header[8:16]
		offset := binary.BigEndian.Uint64(header[16:24])
		length := binary.BigEndian.Uint32(header[24:28])

		switch cmdType {
		case nbdCmdRead:
			sess.handleRead(handle, offset, length)
		case nbdCmdWrite:
			sess.handleWrite(handle, offset, length)
		case nbdCmdFlush:
			sess.sendReply(handle, nbdErrNone, nil)
		case nbdCmdDisc:
			sess.server.logger.Printf("Client disconnected")
			return nil
		case nbdCmdTrim:
			sess.sendReply(handle, nbdErrNone, nil)
		default:
			sess.server.logger.Printf("Unknown command: %d", cmdType)
			sess.sendReply(handle, nbdErrInval, nil)
		}
	}
}

func (sess *session) handleRead(handle []byte, offset uint64, length uint32) {
	exp := sess.export

	if offset+uint64(length) > uint64(exp.Size) {
		sess.sendReply(handle, nbdErrInval, nil)
		return
	}

	data := make([]byte, length)
	n, err := exp.Reader.ReadAt(data, int64(offset))

	if err != nil && err != io.EOF {
		sess.server.logger.Printf("Read error at offset %d: %v", offset, err)
		sess.sendReply(handle, nbdErrIO, nil)
		return
	}

	// Zero-fill if we read less than requested
	for i := n; i < int(length); i++ {
		data[i] = 0
	}

	sess.sendReply(handle, nbdErrNone, data)
}

func (sess *session) handleWrite(handle []byte, offset uint64, length uint32) {
	exp := sess.export

	if exp.Writer == nil {
		io.CopyN(io.Discard, sess.conn, int64(length))
		sess.sendReply(handle, nbdErrPerm, nil)
		return
	}

	if offset+uint64(length) > uint64(exp.Size) {
		io.CopyN(io.Discard, sess.conn, int64(length))
		sess.sendReply(handle, nbdErrInval, nil)
		return
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(sess.conn, data); err != nil {
		sess.server.logger.Printf("Failed to read write data: %v", err)
		return
	}

	_, err := exp.Writer.WriteAt(data, int64(offset))
	if err != nil {
		sess.server.logger.Printf("Write error at offset %d: %v", offset, err)
		sess.sendReply(handle, nbdErrIO, nil)
		return
	}

	sess.sendReply(handle, nbdErrNone, nil)
}

func (sess *session) sendReply(handle []byte, errCode uint32, data []byte) {
	reply := make([]byte, 16+len(data))
	binary.BigEndian.PutUint32(reply[0:4], nbdReplyMagicSimple)
	binary.BigEndian.PutUint32(reply[4:8], errCode)
	copy(reply[8:16], handle)
	if len(data) > 0 {
		copy(reply[16:], data)
	}
	sess.conn.Write(reply)
}
