package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/novartc/sipbench/ebpf"
	"github.com/novartc/sipbench/pkg"
	"github.com/novartc/sipbench/pkg/distributor"
	"github.com/novartc/sipbench/pkg/xdp"
	"github.com/novartc/sipbench/rtp"
)

var (
	errNoFreePorts = errors.New("no free ports")
)

type BaseUaConfig struct {
	Interface string
	// rtp port range: [ RtpPortMin, RtpPortMax )
	RtpPortMin     int
	RtpPortMax     int
	RingingTimeout string
	AnswerTimeout  string
	EarlyMedia     string
	AnswerMedia    string
	XDPSize        int
}

type baseUa struct {
	cfg                   BaseUaConfig
	ctx                   context.Context
	cancel                context.CancelFunc
	dialogs               sync.Map
	ports                 sync.Map
	nextPort              uint16
	portsUsed             atomic.Int64
	portsMax              int64
	ringingTimeRange      pkg.TimeRange
	answerTimeRange       pkg.TimeRange
	EarlyMedia            []rtp.Packet
	AnswerMedia           []rtp.Packet
	ebpf                  ebpf.Interface
	xdpSocketsLB          distributor.Interface[*xdp.Socket]
	metricsCurrentDialogs atomic.Int64
	metricsDialogs        atomic.Uint64
}

func newBaseUa(cfg BaseUaConfig, ebpf ebpf.Interface) (*baseUa, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ua := &baseUa{
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		nextPort: uint16(cfg.RtpPortMin),
		portsMax: int64(cfg.RtpPortMax - cfg.RtpPortMin),
		ebpf:     ebpf,
	}

	var err error
	ua.ringingTimeRange, err = pkg.NewTimeRange(cfg.RingingTimeout)
	if err != nil {
		return nil, err
	}
	ua.answerTimeRange, err = pkg.NewTimeRange(cfg.AnswerTimeout)
	if err != nil {
		return nil, err
	}

	if cfg.EarlyMedia != "" {
		ua.EarlyMedia, err = rtp.LoadRTP(cfg.EarlyMedia)
		if err != nil {
			return nil, err
		}
		slog.Info("load early media", "name", cfg.EarlyMedia, "count", len(ua.EarlyMedia))
	}
	if cfg.AnswerMedia != "" {
		ua.AnswerMedia, err = rtp.LoadRTP(cfg.AnswerMedia)
		if err != nil {
			return nil, err
		}
		slog.Info("load answer media", "name", cfg.AnswerMedia, "count", len(ua.AnswerMedia))
	}

	if err := ua.initXDP(cfg.Interface); err != nil {
		return nil, err
	}
	go ua.metricsOutput()
	return ua, nil
}

func (u *baseUa) initXDP(name string) error {
	ifindex, err := getIfindexByName(name)
	if err != nil {
		return err
	}

	if err := u.ebpf.BindInterfaces([]int{ifindex}); err != nil {
		return err
	}

	var xdpSockets []*xdp.Socket
	for i := 0; i < 16; i++ {
		socket, err := xdp.NewSocket(ifindex, i, &xdp.SocketOptions{
			NumFrames:              u.cfg.XDPSize,
			FrameSize:              2048,
			FillRingNumDescs:       8,
			CompletionRingNumDescs: u.cfg.XDPSize / 2,
			RxRingNumDescs:         8,
			TxRingNumDescs:         u.cfg.XDPSize / 2,
			TxStep:                 500 * time.Microsecond,
		})
		if err != nil {
			// queue doesn't exist
			if errors.Is(err, xdp.ErrBindFailed) {
				break
			}
			return fmt.Errorf("failed to create xdp socket[%d/%d]: %v", ifindex, i, err)
		}
		slog.Info("create xdp socket", "ifindex", ifindex, "queue", i)
		xdpSockets = append(xdpSockets, socket)
	}
	u.xdpSocketsLB = distributor.NewRoundRobin(xdpSockets)

	return nil
}

func (u *baseUa) getPort() (uint16, error) {
	if u.portsMax-u.portsUsed.Load() < 10 {
		return 0, errNoFreePorts
	}

	for {
		port := u.nextPort
		_, loaded := u.ports.LoadOrStore(port, struct{}{})
		u.nextPort += 2
		if u.nextPort >= uint16(u.cfg.RtpPortMax) {
			u.nextPort = uint16(u.cfg.RtpPortMin)
		}
		if loaded {
			continue
		}
		u.portsUsed.Add(2)
		return port, nil
	}
}

func (u *baseUa) freePort(ports ...uint16) {
	for _, p := range ports {
		u.ports.Delete(p)
	}
	u.portsUsed.Add(int64(-2 * len(ports)))
}

func (u *baseUa) metricsOutput() {
	t := time.NewTicker(10 * time.Second)
	for range t.C {
		slog.Warn(
			"call stats",
			slog.Uint64("calls", uint64(u.metricsCurrentDialogs.Load())),
			slog.Uint64("total_calls", u.metricsDialogs.Load()),
		)
	}
}

func (u *baseUa) Close() error {
	return nil
}
