package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"mime"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/novartc/sipbench/pkg/randutils"
)

type DialogUas struct {
	ctx             context.Context
	callId          string
	uas             *Uas
	localAudioPorts []uint16
	session         *sipgo.DialogServerSession
	media           *media
	shouldAnswer    bool
	done            chan struct{}
	logger          *slog.Logger
}

func (u *Uas) NewDialogUas(callId string, session *sipgo.DialogServerSession) *DialogUas {
	dialog := &DialogUas{
		ctx:          context.Background(),
		callId:       callId,
		uas:          u,
		session:      session,
		shouldAnswer: rand.Uint32() < u.answerChance,
		done:         make(chan struct{}),
		logger:       slog.With(slog.String("callId", callId)),
	}

	u.dialogs.Store(callId, dialog)
	u.metricsCurrentDialogs.Add(1)
	u.metricsDialogs.Add(1)
	return dialog
}

func (d *DialogUas) handleState(s sip.DialogState) {
	switch s {
	case sip.DialogStateEnded:
		close(d.done)
	}
}

func (d *DialogUas) onInviteSIPREC(req *sip.Request, tx sip.ServerTransaction) error {
	mediaType, params, err := mime.ParseMediaType(req.ContentType().Value())
	if err != nil {
		if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return err
	}
	if mediaType != "multipart/mixed" {
		if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return fmt.Errorf("unsupported content-type: %s", req.ContentType().Value())
	}
	audioInfos, err := getAudioMediaFromMultipartSDP(req.Body(), params["boundary"])
	if err != nil {
		if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return err
	}

	localSDP := []byte(fmt.Sprintf(sdpSIPREC, d.uas.cfg.NatAddress, d.uas.cfg.NatAddress))
	for range audioInfos {
		port, err := d.uas.getPort()
		if err != nil {
			if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
				d.logger.Error("failed to send response", "error", err)
			}
			return fmt.Errorf("unable to get RTP port: %v", err)
		}
		d.localAudioPorts = append(d.localAudioPorts, port)
		localSDP = append(localSDP, []byte(fmt.Sprintf("m=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n", port))...)
	}

	if err := d.uas.ebpf.Update(d.localAudioPorts...); err != nil {
		return fmt.Errorf("failed to create ebpf rules: %v", err)
	}

	if err := d.session.Respond(
		sip.StatusSessionInProgress,
		"Session Progress",
		localSDP,
		sip.NewHeader("Content-Type", "application/sdp"),
	); err != nil {
		return fmt.Errorf("failed to send response: %v", err)
	}

	var canceled bool
	select {
	case <-d.done:
		// caller hangup
		canceled = true
	case <-time.After(d.uas.ringingTimeRange.Get()):
	}

	if canceled {
		return nil
	}

	if !d.shouldAnswer {
		if err := d.session.Respond(
			sip.StatusBusyHere,
			"Busy Here",
			nil,
		); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return nil
	}

	// answer 200
	if err := d.session.RespondSDP(localSDP); err != nil {
		return fmt.Errorf("failed to send response: %v", err)
	}

	var receiveBye bool
	select {
	case <-d.done:
		// caller hangup
		receiveBye = true
	case <-time.After(d.uas.answerTimeRange.Get()):
	}

	if receiveBye {
		return nil
	}
	if err := d.session.Bye(context.Background()); err != nil {
		return fmt.Errorf("failed to send BYE: %v", err)
	}
	return nil
}

func (d *DialogUas) onInvite(req *sip.Request, tx sip.ServerTransaction) error {
	d.session.OnState(d.handleState)
	if err := d.session.Respond(sip.StatusTrying, "Trying", nil); err != nil {
		return fmt.Errorf("failed to send response: %v", err)
	}

	var audioInfos []mediaInfo
	var err error
	if contentType := req.ContentType().Value(); contentType == "application/sdp" {
		audioInfos, err = getAudioMediaFromSDP(req.Body())
	} else {
		return d.onInviteSIPREC(req, tx)
	}
	if err != nil {
		if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return err
	}

	localAudioPort, err := d.uas.getPort()
	if err != nil {
		if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return fmt.Errorf("unable to get RTP port: %v", err)
	}
	d.localAudioPorts = append(d.localAudioPorts, localAudioPort)

	if err := d.uas.ebpf.Update(localAudioPort, localAudioPort+1); err != nil {
		return fmt.Errorf("failed to create ebpf rules: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	d.media, err = d.uas.newMedia(
		cancelCtx,
		mediaInfo{addr: audioInfos[0].addr, port: audioInfos[0].port},
		mediaInfo{addr: d.uas.cfg.NatAddress, port: int(localAudioPort)},
	)
	if err != nil {
		if err := d.session.Respond(sip.StatusInternalServerError, "Internal Server Error", nil); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return fmt.Errorf("unable to init media: %v", err)
	}

	localSDP := []byte(fmt.Sprintf(sdpBase, d.uas.cfg.NatAddress, d.uas.cfg.NatAddress, localAudioPort))
	if err := d.session.Respond(
		sip.StatusSessionInProgress,
		"Session Progress",
		localSDP,
		sip.NewHeader("Content-Type", "application/sdp"),
	); err != nil {
		return fmt.Errorf("failed to send response: %v", err)
	}

	// ringing 183
	if err := d.media.playback(d.uas.EarlyMedia); err != nil {
		return fmt.Errorf("failed to play early media: %v", err)
	}
	var canceled bool
	select {
	case <-d.done:
		// caller hangup
		canceled = true
	case <-time.After(d.uas.ringingTimeRange.Get()):
	}

	if canceled {
		return nil
	}

	if !d.shouldAnswer {
		if err := d.session.Respond(
			sip.StatusBusyHere,
			"Busy Here",
			nil,
		); err != nil {
			d.logger.Error("failed to send response", "error", err)
		}
		return nil
	}

	// answer 200
	if err := d.session.RespondSDP(localSDP); err != nil {
		return fmt.Errorf("failed to send response: %v", err)
	}

	if err := d.media.playback(d.uas.AnswerMedia); err != nil {
		return fmt.Errorf("failed to play answer media: %v", err)
	}
	var receiveBye bool
	select {
	case <-d.done:
		// caller hangup
		receiveBye = true
	case <-time.After(d.uas.answerTimeRange.Get()):
	}

	if receiveBye {
		return nil
	}
	if err := d.session.Bye(cancelCtx); err != nil {
		return fmt.Errorf("failed to send BYE: %v", err)
	}
	return nil
}

func (d *DialogUas) onBye(req *sip.Request, tx sip.ServerTransaction) error {
	if err := d.session.ReadBye(req, tx); err != nil {
		return err
	}
	return nil
}

func (d *DialogUas) Close() error {
	var txPkts, txLossPkts, txBytes uint64
	var rxPkts, rxBytes []uint64

	if len(d.localAudioPorts) > 0 {
		d.uas.freePort(d.localAudioPorts...)
		// FIXME: delete RTCP ports
		rtpInfos, err := d.uas.ebpf.LookupAndDelete(d.localAudioPorts...)
		if err != nil {
			d.logger.Warn("failed to get rtp infos", "error", err)
		} else {
			for _, info := range rtpInfos {
				rxPkts = append(rxPkts, info.Pkts)
				rxBytes = append(rxBytes, info.Bytes)
			}
		}
	}
	if d.media != nil {
		txPkts = d.media.txPkts
		txBytes = d.media.txBytes
		txLossPkts = d.media.txLossPkts
		_ = d.media.Close()
	}
	d.logger.Info(
		"rtp stats",
		"ports", d.localAudioPorts,
		"rx_pkts", rxPkts,
		"rx_bytes", rxBytes,
		"tx_pkts", txPkts,
		"tx_loss_pkts", txLossPkts,
		"tx_bytes", txBytes,
	)

	d.uas.dialogs.Delete(d.callId)
	d.uas.metricsCurrentDialogs.Add(-1)
	return nil
}

type DialogUac struct {
	ctx            context.Context
	cancel         context.CancelFunc
	callId         string
	uac            *Uac
	localAudioPort uint16
	session        *sipgo.DialogClientSession
	media          *media
	done           chan struct{}
	logger         *slog.Logger
}

func (u *Uac) NewDialogUac() *DialogUac {
	callId := randutils.RandString(10)
	dialog := &DialogUac{
		ctx:    context.Background(),
		callId: callId,
		uac:    u,
		done:   make(chan struct{}),
		logger: slog.With(slog.String("callId", callId)),
	}
	u.dialogs.Store(dialog.callId, dialog)
	u.metricsCurrentDialogs.Add(1)
	u.metricsDialogs.Add(1)
	return dialog
}

func (d *DialogUac) MakeCall(recipient sip.Uri) error {
	var err error
	d.localAudioPort, err = d.uac.getPort()
	if err != nil {
		return fmt.Errorf("unable to get RTP port: %v", err)
	}

	if err := d.uac.ebpf.Update(d.localAudioPort, d.localAudioPort+1); err != nil {
		return fmt.Errorf("failed to create ebpf rules: %v", err)
	}

	localSDP := []byte(fmt.Sprintf(sdpBase, d.uac.cfg.Address, d.uac.cfg.Address, d.localAudioPort))
	callIdHdr := sip.CallIDHeader(d.callId)
	d.session, err = d.uac.dialogUA.Invite(
		context.TODO(),
		recipient,
		localSDP,
		&sip.FromHeader{
			Address: d.uac.dialogUA.ContactHDR.Address,
			Params:  sip.HeaderParams{"tag": randutils.RandString(5)},
		},
		&sip.ToHeader{Address: recipient},
		&callIdHdr,
		sip.NewHeader("Content-Type", "application/sdp"),
	)
	if err != nil {
		return fmt.Errorf("failed to send INVITE: %v", err)
	}
	d.session.OnState(d.handleState)

	d.ctx, d.cancel = context.WithCancel(context.Background())
	if err := d.session.WaitAnswer(d.ctx, sipgo.AnswerOptions{OnResponse: d.onResponse}); err != nil {
		if !errors.Is(err, context.Canceled) {
			d.cancel()
		}
		return fmt.Errorf("error occurred while waiting for answer: %v", err)
	}
	defer d.cancel()

	if err := d.session.Ack(d.ctx); err != nil {
		return fmt.Errorf("failed to send ACK: %v", err)
	}

	if err := d.media.playback(d.uac.AnswerMedia); err != nil {
		return fmt.Errorf("failed to play answer media: %v", err)
	}

	var receiveBye bool
	select {
	case <-d.done:
		// caller hangup
		receiveBye = true
	case <-time.After(d.uac.answerTimeRange.Get()):
	}

	if err := d.media.stop(); err != nil {
		d.logger.Warn("failed to stop playback", "error", err)
	}

	if receiveBye {
		return nil
	}
	if err := d.session.Bye(d.ctx); err != nil {
		return fmt.Errorf("failed to send BYE: %v", err)
	}
	return nil
}

func (d *DialogUac) handleState(s sip.DialogState) {
	switch s {
	case sip.DialogStateEnded:
		close(d.done)
	}
}

func (d *DialogUac) onResponse(res *sip.Response) error {
	switch res.StatusCode {
	case sip.StatusSessionInProgress, sip.StatusRinging:
		if len(res.Body()) == 0 {
			return nil
		}

		if d.media == nil {
			audioInfos, err := getAudioMediaFromSDP(res.Body())
			if err != nil {
				d.logger.Error("failed to get media infos from sdp", "error", err)
				d.cancel()
				return nil
			}
			d.media, err = d.uac.newMedia(
				d.ctx,
				mediaInfo{addr: audioInfos[0].addr, port: audioInfos[0].port},
				mediaInfo{addr: d.uac.cfg.Address, port: int(d.localAudioPort)},
			)
			if err != nil {
				d.logger.Error("unabled to init media", "error", err)
				d.cancel()
				return nil
			}

			if err := d.media.playback(d.uac.EarlyMedia); err != nil {
				d.logger.Error("failed to play early media", "error", err)
				d.cancel()
				return nil
			}
		}
	case sip.StatusOK:
		if d.media == nil {
			audioInfos, err := getAudioMediaFromSDP(res.Body())
			if err != nil {
				d.logger.Error("failed to get media infos from sdp", "error", err)
				d.cancel()
				return nil
			}
			d.media, err = d.uac.newMedia(
				d.ctx,
				mediaInfo{addr: audioInfos[0].addr, port: audioInfos[0].port},
				mediaInfo{addr: d.uac.cfg.Address, port: int(d.localAudioPort)},
			)
			if err != nil {
				d.logger.Error("unabled to init media", "error", err)
				d.cancel()
				return nil
			}
		}
	}
	return nil
}

func (d *DialogUac) onBye(req *sip.Request, tx sip.ServerTransaction) error {
	if err := d.session.ReadBye(req, tx); err != nil {
		return err
	}
	return nil
}

func (d *DialogUac) Close() error {
	var rxPkts, txPkts, rxBytes, txBytes uint64

	if d.localAudioPort != 0 {
		d.uac.freePort(d.localAudioPort)
		rtpInfos, err := d.uac.ebpf.LookupAndDelete(d.localAudioPort)
		if err != nil {
			d.logger.Warn("failed to get rtp infos", "error", err)
		}
		rxPkts = rtpInfos[0].Pkts
		rxBytes = rtpInfos[0].Bytes
	}
	if d.media != nil {
		txPkts = d.media.txPkts
		txBytes = d.media.txBytes
		_ = d.media.Close()
	}
	d.logger.Info(
		"rtp stats",
		"port", d.localAudioPort,
		"rx_pkts", rxPkts,
		"rx_bytes", rxBytes,
		"tx_pkts", txPkts,
		"tx_bytes", txBytes,
	)

	d.uac.dialogs.Delete(d.callId)
	d.uac.metricsCurrentDialogs.Add(-1)
	return nil
}
