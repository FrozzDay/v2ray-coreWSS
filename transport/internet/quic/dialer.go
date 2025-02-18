package quic

import (
	"context"
	"sync"
	"time"

	"github.com/lucas-clemente/quic-go"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/task"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
	"github.com/v2fly/v2ray-core/v5/transport/internet/tls"
)

type connectionContext struct {
	rawConn *sysConn
	conn    quic.Connection
}

var errConnectionClosed = newError("connection closed")

func (c *connectionContext) openStream(destAddr net.Addr) (*interConn, error) {
	if !isActive(c.conn) {
		return nil, errConnectionClosed
	}

	stream, err := c.conn.OpenStream()
	if err != nil {
		return nil, err
	}

	conn := &interConn{
		stream: stream,
		local:  c.conn.LocalAddr(),
		remote: destAddr,
	}

	return conn, nil
}

type clientConnections struct {
	access  sync.Mutex
	conns   map[net.Destination][]*connectionContext
	cleanup *task.Periodic
}

func isActive(s quic.Connection) bool {
	select {
	case <-s.Context().Done():
		return false
	default:
		return true
	}
}

func removeInactiveConnections(conns []*connectionContext) []*connectionContext {
	activeConnections := make([]*connectionContext, 0, len(conns))
	for _, s := range conns {
		if isActive(s.conn) {
			activeConnections = append(activeConnections, s)
			continue
		}
		if err := s.conn.CloseWithError(0, ""); err != nil {
			newError("failed to close connection").Base(err).WriteToLog()
		}
		if err := s.rawConn.Close(); err != nil {
			newError("failed to close raw connection").Base(err).WriteToLog()
		}
	}

	if len(activeConnections) < len(conns) {
		return activeConnections
	}

	return conns
}

func openStream(conns []*connectionContext, destAddr net.Addr) *interConn {
	for _, s := range conns {
		if !isActive(s.conn) {
			continue
		}

		conn, err := s.openStream(destAddr)
		if err != nil {
			continue
		}

		return conn
	}

	return nil
}

func (s *clientConnections) cleanConnections() error {
	s.access.Lock()
	defer s.access.Unlock()

	if len(s.conns) == 0 {
		return nil
	}

	newConnMap := make(map[net.Destination][]*connectionContext)

	for dest, conns := range s.conns {
		conns = removeInactiveConnections(conns)
		if len(conns) > 0 {
			newConnMap[dest] = conns
		}
	}

	s.conns = newConnMap
	return nil
}

func (s *clientConnections) openConnection(ctx context.Context, destAddr net.Addr, config *Config, tlsConfig *tls.Config, sockopt *internet.SocketConfig) (internet.Connection, error) {
	s.access.Lock()
	defer s.access.Unlock()

	if s.conns == nil {
		s.conns = make(map[net.Destination][]*connectionContext)
	}

	dest := net.DestinationFromAddr(destAddr)

	var conns []*connectionContext
	if s, found := s.conns[dest]; found {
		conns = s
	}

	{
		conn := openStream(conns, destAddr)
		if conn != nil {
			return conn, nil
		}
	}

	conns = removeInactiveConnections(conns)

	newError("dialing QUIC to ", dest).WriteToLog()

	rawConn, err := internet.DialSystem(ctx, dest, sockopt)
	if err != nil {
		return nil, newError("failed to dial to dest: ", err).AtWarning().Base(err)
	}

	quicConfig := &quic.Config{
		ConnectionIDLength:   12,
		HandshakeIdleTimeout: time.Second * 8,
		MaxIdleTimeout:       time.Second * 30,
		KeepAlivePeriod:      time.Second * 15,
	}

	udpConn, _ := rawConn.(*net.UDPConn)
	if udpConn == nil {
		udpConn = rawConn.(*internet.PacketConnWrapper).Conn.(*net.UDPConn)
	}
	sysConn, err := wrapSysConn(udpConn, config)
	if err != nil {
		rawConn.Close()
		return nil, err
	}

	conn, err := quic.DialContext(context.Background(), sysConn, destAddr, "", tlsConfig.GetTLSConfig(tls.WithDestination(dest)), quicConfig)
	if err != nil {
		sysConn.Close()
		return nil, err
	}

	context := &connectionContext{
		conn:    conn,
		rawConn: sysConn,
	}
	s.conns[dest] = append(conns, context)
	return context.openStream(destAddr)
}

var client clientConnections

func init() {
	client.conns = make(map[net.Destination][]*connectionContext)
	client.cleanup = &task.Periodic{
		Interval: time.Minute,
		Execute:  client.cleanConnections,
	}
	common.Must(client.cleanup.Start())
}

func Dial(ctx context.Context, dest net.Destination, streamSettings *internet.MemoryStreamConfig) (internet.Connection, error) {
	tlsConfig := tls.ConfigFromStreamSettings(streamSettings)
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			ServerName:    internalDomain,
			AllowInsecure: true,
		}
	}

	var destAddr *net.UDPAddr
	if dest.Address.Family().IsIP() {
		destAddr = &net.UDPAddr{
			IP:   dest.Address.IP(),
			Port: int(dest.Port),
		}
	} else {
		addr, err := net.ResolveUDPAddr("udp", dest.NetAddr())
		if err != nil {
			return nil, err
		}
		destAddr = addr
	}

	config := streamSettings.ProtocolSettings.(*Config)

	return client.openConnection(ctx, destAddr, config, tlsConfig, streamSettings.SocketSettings)
}

func init() {
	common.Must(internet.RegisterTransportDialer(protocolName, Dial))
}
