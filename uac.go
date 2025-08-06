package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/novartc/sipbench/arp"
	"github.com/novartc/sipbench/ebpf"
	"github.com/novartc/sipbench/routing"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
)

type uacConfig struct {
	BaseUaConfig `json:",inline"`
	Address      string
	Port         int
	TotalCalls   int
	MaxCalls     int
	Rate         int
	User         string
}

type Uac struct {
	*baseUa
	cfg      uacConfig
	dialogUA sipgo.DialogUA
}

func newUacCommand() *cobra.Command {
	var opts uacConfig

	cmd := &cobra.Command{
		Use:   "uac",
		Short: "uac",
		Long:  "uac",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			arp.MustInit()
			routing.MustInit()
			go startRefreshTimer(time.Minute)

			emgr, err := ebpf.New()
			if err != nil {
				slog.Error("failed to init ebpf", "error", err)
				return
			}

			uac, err := runUac(opts, emgr, args[0])
			if err != nil {
				slog.Error("failed to init uac", "error", err)
				return
			}

			waitShutdown(uac.ctx, uac, emgr)
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
	set.IntVar(&opts.TotalCalls, "total-calls", 0, "the total number of calls")
	set.IntVar(&opts.MaxCalls, "max-calls", 1, "the maximum number of online calls")
	set.IntVar(&opts.Rate, "rate", 10, "the number of calls per second")
	set.StringVar(&opts.User, "user", "", "sip remote user")
	return cmd
}

func runUac(cfg uacConfig, ebpf ebpf.Interface, dest string) (*Uac, error) {
	recipient, err := parseIPPort(dest)
	if err != nil {
		return nil, err
	}
	recipient.User = cfg.User

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

	localContact := sip.ContactHeader{Address: sip.Uri{User: "sipbench", Host: cfg.Address, Port: cfg.Port}}

	bua, err := newBaseUa(cfg.BaseUaConfig, ebpf)
	if err != nil {
		return nil, err
	}
	u := &Uac{
		baseUa:   bua,
		cfg:      cfg,
		dialogUA: sipgo.DialogUA{Client: client, ContactHDR: localContact},
	}

	sip.UDPMTUSize = 2048
	server.OnBye(u.onBye)

	go func() {
		if err := server.ListenAndServe(context.Background(), "udp", addr); err != nil {
			_ = ua.Close()
			_ = client.Close()
			panic(err)
		}
	}()

	time.Sleep(300 * time.Millisecond)

	go u.startCallMaker(u.ctx, recipient)

	return u, nil
}

func (u *Uac) startCallMaker(ctx context.Context, recipient sip.Uri) {
	r := rate.NewLimiter(rate.Limit(u.cfg.Rate), u.cfg.Rate)
	calls := make(chan struct{}, u.cfg.MaxCalls)

	var i uint64
loop:
	for {
		if u.cfg.TotalCalls != 0 && i == uint64(u.cfg.TotalCalls) {
			u.cancel()
			break
		}
		i++

		select {
		case <-ctx.Done():
			break loop
		default:
		}
		calls <- struct{}{}

		if err := r.Wait(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			slog.Error("failed to wait call rate limiter", "error", err)
			return
		}

		go func() {
			d := u.NewDialogUac()
			defer d.Close()
			if err := d.MakeCall(recipient); err != nil {
				slog.Warn("failed to make call", "error", err)
			}
			<-calls
		}()
	}
}

func (u *Uac) onBye(req *sip.Request, tx sip.ServerTransaction) {
	got, ok := u.dialogs.Load(req.CallID().Value())
	if !ok {
		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
			slog.Error("failed to respond BYE", "error", err, slog.String("callId", req.CallID().Value()))
		}
		return
	}
	dialog := got.(*DialogUac)
	if err := dialog.onBye(req, tx); err != nil {
		slog.Error("failed to handle BYE", "error", err, slog.String("callId", req.CallID().Value()))
		return
	}
}

func (u *Uac) Close() error {
	select {
	case <-u.ctx.Done():
	default:
		u.cancel()
	}

	now := time.Now()
	for {
		current := u.metricsCurrentDialogs.Load()
		if current == 0 || time.Now().Sub(now) > 80*time.Second {
			break
		}
		slog.Warn("wait for all calls to end", "calls", current)
		time.Sleep(time.Second)
	}
	return u.baseUa.Close()
}

func parseIPPort(raw string) (sip.Uri, error) {
	uri := sip.Uri{
		Host: raw,
		Port: 5060,
	}

	i := strings.IndexByte(raw, ':')
	if i != -1 {
		uri.Host = raw[:i]
		port, err := strconv.Atoi(raw[i+1:])
		if err != nil {
			return sip.Uri{}, fmt.Errorf("unable to parse SIP port: %v", err)
		}
		uri.Port = port
	}
	return uri, nil
}
