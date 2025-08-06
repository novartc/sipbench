package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/novartc/sipbench/arp"
	"github.com/novartc/sipbench/ebpf"
	"github.com/novartc/sipbench/routing"
	"github.com/spf13/cobra"
)

type uasConfig struct {
	BaseUaConfig
	Address      string
	NatAddress   string
	Port         int
	AnswerChance int
}

type Uas struct {
	*baseUa
	cfg          uasConfig
	dialogUA     sipgo.DialogUA
	answerChance uint32
}

func newUasCommand() *cobra.Command {
	var opts uasConfig

	cmd := &cobra.Command{
		Use:   "uas",
		Short: "uas",
		Long:  "uas",
		Run: func(cmd *cobra.Command, args []string) {
			arp.MustInit()
			routing.MustInit()
			go startRefreshTimer(time.Minute)

			emgr, err := ebpf.New()
			if err != nil {
				slog.Error("failed to init ebpf", "error", err)
				return
			}

			uas, err := runUas(opts, emgr)
			if err != nil {
				slog.Error("failed to init uas", "error", err)
				return
			}

			waitShutdown(uas.ctx, uas, emgr)
		},
	}

	set := cmd.Flags()
	set.StringVar(&opts.BaseUaConfig.Interface, "interface", "eth0", "interface name")
	set.IntVar(&opts.BaseUaConfig.RtpPortMin, "min-rtp-port", 20000, "min rtp port")
	set.IntVar(&opts.BaseUaConfig.RtpPortMax, "max-rtp-port", 60000, "max rtp port")
	set.StringVar(&opts.BaseUaConfig.RingingTimeout, "ringing-timeout", "5", "ringing timeout in seconds")
	set.StringVar(&opts.BaseUaConfig.AnswerTimeout, "answer-timeout", "20", "answer timeout in seconds")
	set.StringVar(&opts.BaseUaConfig.EarlyMedia, "early-media", "30", "The content of early media playback (can be white noise, pcap file, pcm file), the default is 30 seconds of white noise")
	set.StringVar(&opts.BaseUaConfig.AnswerMedia, "answer-media", "30", "answer media playback content (can be white noise, pcap files, pcm files), the default is 30 seconds of white noise")
	set.IntVar(&opts.BaseUaConfig.XDPSize, "xdp-size", 4096, "xdp ring size(must be a power of 2)")
	set.StringVar(&opts.Address, "address", "", "listen address")
	set.IntVar(&opts.Port, "port", 5060, "listen port")
	set.StringVar(&opts.NatAddress, "public-addr", "", "nat address")
	set.IntVar(&opts.AnswerChance, "answer-chance", 100, "answer chance")
	return cmd
}

func runUas(cfg uasConfig, ebpf ebpf.Interface) (*Uas, error) {
	if cfg.AnswerChance < 0 || cfg.AnswerChance > 100 {
		return nil, fmt.Errorf("answer chance must be between 0 and 100")
	}
	if cfg.NatAddress == "" {
		cfg.NatAddress = cfg.Address
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		return nil, err
	}

	server, err := sipgo.NewServer(ua)
	if err != nil {
		_ = ua.Close()
		return nil, err
	}

	addr := cfg.Address + ":" + strconv.Itoa(cfg.Port)
	client, err := sipgo.NewClient(ua, sipgo.WithClientAddr(addr), sipgo.WithClientConnectionAddr(addr))
	if err != nil {
		_ = server.Close()
		_ = ua.Close()
		return nil, err
	}

	localContact := sip.ContactHeader{Address: sip.Uri{User: "sipbench", Host: cfg.NatAddress, Port: cfg.Port}}

	bua, err := newBaseUa(cfg.BaseUaConfig, ebpf)
	if err != nil {
		return nil, err
	}
	u := &Uas{
		baseUa:       bua,
		cfg:          cfg,
		dialogUA:     sipgo.DialogUA{Client: client, ContactHDR: localContact},
		answerChance: uint32(float64(cfg.AnswerChance) / 100 * float64(math.MaxUint32)),
	}

	sip.UDPMTUSize = 2048
	server.OnInvite(u.onInvite)
	server.OnAck(u.onAck)
	server.OnBye(u.onBye)
	server.OnOptions(u.onOptions)

	go func() {
		if err := server.ListenAndServe(context.Background(), "udp", addr); err != nil {
			_ = ua.Close()
			_ = client.Close()
			panic(err)
		}
	}()

	return u, nil
}

func (u *Uas) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	callId := req.CallID().Value()
	session, err := u.dialogUA.ReadInvite(req, tx)
	if err != nil {
		slog.Error("failed to read INVITE", "error", err, slog.String("callId", callId))
		return
	}
	dialog := u.NewDialogUas(callId, session)
	defer dialog.Close()

	if err := dialog.onInvite(req, tx); err != nil {
		slog.Error("failed to handle INVITE", "error", err, slog.String("callId", callId))
		return
	}
}

func (u *Uas) onAck(req *sip.Request, tx sip.ServerTransaction) {
	got, ok := u.dialogs.Load(req.CallID().Value())
	if !ok {
		return
	}
	dialog := got.(*DialogUas)
	if err := dialog.session.ReadAck(req, tx); err != nil {
		slog.Error("failed to read ACK", "error", err, slog.String("callId", req.CallID().Value()))
		return
	}
}

func (u *Uas) onBye(req *sip.Request, tx sip.ServerTransaction) {
	got, ok := u.dialogs.Load(req.CallID().Value())
	if !ok {
		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
			slog.Error("failed to respond BYE", "error", err, slog.String("callId", req.CallID().Value()))
		}
		return
	}
	dialog := got.(*DialogUas)
	if err := dialog.onBye(req, tx); err != nil {
		slog.Error("failed to handle BYE", "error", err, slog.String("callId", req.CallID().Value()))
		return
	}
}

func (u *Uas) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
		slog.Error("failed to respond OPTIONS", "error", err, slog.String("callId", req.CallID().Value()))
	}
}

func (u *Uas) Close() error {
	return u.baseUa.Close()
}

func getIfindexByName(name string) (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return -1, err
	}

	for _, iface := range ifaces {
		if iface.Name == name {
			return iface.Index, nil
		}
	}
	return -1, fmt.Errorf("interface %s not found", name)
}
