package aoe

import (
	"net"
	"syscall"
	"time"

	"github.com/coreos/torus/block"

	"github.com/coreos/pkg/capnslog"
	"github.com/mdlayher/aoe"
	"github.com/mdlayher/raw"
)

var (
	clog          = capnslog.NewPackageLogger("github.com/coreos/torus", "aoe")
	broadcastAddr = net.HardwareAddr([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
)

type Server struct {
	dfs *block.BlockVolume

	dev Device

	major uint16
	minor uint8
}

// ServerOptions specifies options for a Server.
type ServerOptions struct {
	// Major and Minor specify the major and minor address of an AoE server.
	// Typically, all AoE devices on a single server will share the same
	// major address, but have different minor addresses.
	//
	// It is important to note that all AoE servers on the same layer 2
	// network must have different major and minor addresses.
	Major uint16
	Minor uint8
}

// DefaultServerOptions is the default ServerOptions configuration used
// by NewServer when none is specified.
var DefaultServerOptions = &ServerOptions{
	Major: 1,
	Minor: 1,
}

// NewServer creates a new Server which utilizes the specified block volume.
// If options is nil, DefaultServerOptions will be used.
func NewServer(b *block.BlockVolume, options *ServerOptions) (*Server, error) {
	if options == nil {
		options = DefaultServerOptions
	}

	f, err := b.OpenBlockFile()
	if err != nil {
		return nil, err
	}

	f.Sync()

	fd := &FileDevice{f}

	as := &Server{
		dfs:   b,
		dev:   fd,
		major: options.Major,
		minor: options.Minor,
	}

	return as, nil
}

func (s *Server) advertise(iface *Interface) error {
	// little hack to trigger a broadcast
	from := &raw.Addr{
		HardwareAddr: broadcastAddr,
	}

	fr := &Frame{
		Header: aoe.Header{
			Command: aoe.CommandQueryConfigInformation,
			Arg: &aoe.ConfigArg{
				Command: aoe.ConfigCommandRead,
			},
		},
	}

	_, err := s.handleFrame(from, iface, fr)
	return err
}

func (s *Server) Serve(iface *Interface) error {
	clog.Tracef("beginning server loop on %+v", iface)

	// cheap sync proc, should stop when server is shut off
	go func() {
		for {
			if err := s.dev.Sync(); err != nil {
				clog.Warningf("failed to sync %s: %v", s.dev, err)
			}

			time.Sleep(5 * time.Second)
		}
	}()

	// broadcast ourselves
	if err := s.advertise(iface); err != nil {
		clog.Errorf("advertisement failed: %v", err)
		return err
	}

	for {
		payload := make([]byte, iface.MTU)
		n, addr, err := iface.ReadFrom(payload)
		if err != nil {
			clog.Errorf("ReadFrom failed: %v", err)
			// will be syscall.EBADF if the conn from raw closed
			if err == syscall.EBADF {
				break
			}
			return err
		}

		// resize payload
		payload = payload[:n]

		var f Frame
		if err := f.UnmarshalBinary(payload); err != nil {
			clog.Errorf("Failed to unmarshal frame: %v", err)
			continue
		}

		clog.Debugf("recv %d %s %+v", n, addr, f.Header)
		//clog.Debugf("recv arg %+v", f.Header.Arg)

		s.handleFrame(addr, iface, &f)
	}

	return nil
}

func (s *Server) handleFrame(from net.Addr, iface *Interface, f *Frame) (int, error) {
	hdr := &f.Header

	sender := &FrameSender{
		orig:  f,
		dst:   from.(*raw.Addr).HardwareAddr,
		src:   iface.HardwareAddr,
		conn:  iface.PacketConn,
		major: s.major,
		minor: s.minor,
	}

	switch hdr.Command {
	case aoe.CommandIssueATACommand:
		n, err := aoe.ServeATA(sender, hdr, s.dev)
		if err != nil {
			clog.Errorf("ServeATA failed: %v", err)
			switch err {
			case aoe.ErrInvalidATARequest:
				return sender.SendError(aoe.ErrorBadArgumentParameter)
			default:
				return sender.SendError(aoe.ErrorDeviceUnavailable)
			}
		}

		return n, nil
	case aoe.CommandQueryConfigInformation:
		cfgarg := f.Header.Arg.(*aoe.ConfigArg)
		clog.Debugf("cfgarg: %+v", cfgarg)

		switch cfgarg.Command {
		case aoe.ConfigCommandRead:
			hdr.Arg = &aoe.ConfigArg{
				// if < 2, linux aoe handles it poorly
				BufferCount:     2,
				FirmwareVersion: 0,
				// naive, but works.
				SectorCount:  uint8(iface.MTU / 512),
				Version:      1,
				Command:      aoe.ConfigCommandRead,
				StringLength: 0,
				String:       []byte{},
			}

			return sender.Send(hdr)
		}

		return sender.SendError(aoe.ErrorUnrecognizedCommandCode)
	case aoe.CommandMACMaskList:
		fallthrough
	case aoe.CommandReserveRelease:
		fallthrough
	default:
		return sender.SendError(aoe.ErrorUnrecognizedCommandCode)
	}
}

func (s *Server) Close() error {
	return s.dev.Close()
}
