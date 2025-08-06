package xdp

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var ErrBindFailed = errors.New("syscall.Bind SockaddrXDP failed")

// DefaultSocketOptions is the default SocketOptions used by an xdp.Socket created without specifying options.
var DefaultSocketOptions = SocketOptions{
	NumFrames:              128,
	FrameSize:              2048,
	FillRingNumDescs:       64,
	CompletionRingNumDescs: 64,
	RxRingNumDescs:         64,
	TxRingNumDescs:         64,
	TxStep:                 100 * time.Microsecond,
}

type umemRing struct {
	Producer *uint32
	Consumer *uint32
	Descs    []uint64
}

type rxTxRing struct {
	Producer *uint32
	Consumer *uint32
	Descs    []Desc
}

// A Socket is an implementation of the AF_XDP Linux socket type for reading packets from a device.
type Socket struct {
	fd               int
	umem             []byte
	fillRing         umemRing
	rxRing           rxTxRing
	txRing           rxTxRing
	completionRing   umemRing
	ifindex          int
	numTransmitted   int
	numFilled        int
	freeRXDescs      []bool
	options          SocketOptions
	rxDescs          []Desc
	getRXDescs       []Desc
	txChan           chan Desc
	manualTriggerTX  chan struct{}
	rxIndex, txIndex atomic.Uint64
}

func (xsk *Socket) Ifindex() int { return xsk.ifindex }

// SocketOptions are configuration settings used to bind an XDP socket.
type SocketOptions struct {
	NumFrames              int
	FrameSize              int
	FillRingNumDescs       int
	CompletionRingNumDescs int
	RxRingNumDescs         int
	TxRingNumDescs         int
	TxStep                 time.Duration
}

// Desc represents an XDP Rx/Tx descriptor.
type Desc unix.XDPDesc

// Stats contains various counters of the XDP socket, such as numbers of
// sent/received frames.
type Stats struct {
	// Transmitted is the number of items consumed thus far by the Linux
	// kernel from the Tx ring queue.
	TxProd uint64
	TxCon  uint64

	// Completed is the number of items consumed thus far by the user of
	// this package from the Completion ring queue.
	CompletionProd uint64
	CompletionCon  uint64

	TxChan int
	// KernelStats contains the in-kernel statistics of the corresponding
	// XDP socket, such as the number of invalid descriptors that were
	// submitted into Fill or Tx ring queues.
	KernelStats unix.XDPStatistics
}

// DefaultSocketFlags are the flags which are passed to bind(2) system call
// when the XDP socket is bound, possible values include unix.XDP_SHARED_UMEM,
// unix.XDP_COPY, unix.XDP_ZEROCOPY.
var DefaultSocketFlags uint16

// DefaultXdpFlags are the flags which are passed when the XDP program is
// attached to the network link, possible values include
// unix.XDP_FLAGS_DRV_MODE, unix.XDP_FLAGS_HW_MODE, unix.XDP_FLAGS_SKB_MODE,
// unix.XDP_FLAGS_UPDATE_IF_NOEXIST.
var DefaultXdpFlags uint32

func init() {
	DefaultSocketFlags = 0
	DefaultXdpFlags = 0
}

// NewSocket returns a new XDP socket attached to the network interface which
// has the given interface, and attached to the given queue on that network
// interface.
func NewSocket(ifindex int, QueueID int, options *SocketOptions) (xsk *Socket, err error) {
	if options == nil {
		options = &DefaultSocketOptions
	}
	xsk = &Socket{fd: -1, ifindex: ifindex, options: *options}
	xsk.txIndex.Store(math.MaxUint64)

	xsk.fd, err = syscall.Socket(unix.AF_XDP, syscall.SOCK_RAW, 0)
	if err != nil {
		return nil, fmt.Errorf("syscall.Socket failed: %v", err)
	}

	xsk.umem, err = syscall.Mmap(-1, 0, options.NumFrames*options.FrameSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_POPULATE)
	if err != nil {
		xsk.Close()
		return nil, fmt.Errorf("syscall.Mmap failed: %v", err)
	}

	xdpUmemReg := unix.XDPUmemReg{
		Addr:     uint64(uintptr(unsafe.Pointer(&xsk.umem[0]))),
		Len:      uint64(len(xsk.umem)),
		Size:     uint32(options.FrameSize),
		Headroom: 0,
	}

	var errno syscall.Errno
	var rc uintptr

	rc, _, errno = unix.Syscall6(syscall.SYS_SETSOCKOPT, uintptr(xsk.fd),
		unix.SOL_XDP, unix.XDP_UMEM_REG,
		uintptr(unsafe.Pointer(&xdpUmemReg)),
		unsafe.Sizeof(xdpUmemReg), 0)
	if rc != 0 {
		xsk.Close()
		return nil, fmt.Errorf("unix.SetsockoptUint64 XDP_UMEM_REG failed: %v", errno)
	}

	err = syscall.SetsockoptInt(xsk.fd, unix.SOL_XDP, unix.XDP_UMEM_FILL_RING,
		options.FillRingNumDescs)
	if err != nil {
		xsk.Close()
		return nil, fmt.Errorf("unix.SetsockoptUint64 XDP_UMEM_FILL_RING failed: %v", err)
	}

	err = unix.SetsockoptInt(xsk.fd, unix.SOL_XDP, unix.XDP_UMEM_COMPLETION_RING,
		options.CompletionRingNumDescs)
	if err != nil {
		xsk.Close()
		return nil, fmt.Errorf("unix.SetsockoptUint64 XDP_UMEM_COMPLETION_RING failed: %v", err)
	}

	var rxRing bool
	if options.RxRingNumDescs > 0 {
		err = unix.SetsockoptInt(xsk.fd, unix.SOL_XDP, unix.XDP_RX_RING,
			options.RxRingNumDescs)
		if err != nil {
			xsk.Close()
			return nil, fmt.Errorf("unix.SetsockoptUint64 XDP_RX_RING failed: %v", err)
		}
		rxRing = true
	}

	var txRing bool
	if options.TxRingNumDescs > 0 {
		err = unix.SetsockoptInt(xsk.fd, unix.SOL_XDP, unix.XDP_TX_RING,
			options.TxRingNumDescs)
		if err != nil {
			xsk.Close()
			return nil, fmt.Errorf("unix.SetsockoptUint64 XDP_TX_RING failed: %v", err)
		}
		txRing = true
	}

	if !(rxRing || txRing) {
		return nil, fmt.Errorf("RxRingNumDescs and TxRingNumDescs cannot both be set to zero")
	}

	var offsets unix.XDPMmapOffsets
	var vallen uint32
	vallen = uint32(unsafe.Sizeof(offsets))
	rc, _, errno = unix.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(xsk.fd),
		unix.SOL_XDP, unix.XDP_MMAP_OFFSETS,
		uintptr(unsafe.Pointer(&offsets)),
		uintptr(unsafe.Pointer(&vallen)), 0)
	if rc != 0 {
		xsk.Close()
		return nil, fmt.Errorf("unix.Syscall6 getsockopt XDP_MMAP_OFFSETS failed: %v", errno)
	}

	fillRingSlice, err := syscall.Mmap(xsk.fd, unix.XDP_UMEM_PGOFF_FILL_RING,
		int(offsets.Fr.Desc+uint64(options.FillRingNumDescs)*uint64(unsafe.Sizeof(uint64(0)))),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		xsk.Close()
		return nil, fmt.Errorf("syscall.Mmap XDP_UMEM_PGOFF_FILL_RING failed: %v", err)
	}

	xsk.fillRing.Producer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&fillRingSlice[0])) + uintptr(offsets.Fr.Producer)))
	xsk.fillRing.Consumer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&fillRingSlice[0])) + uintptr(offsets.Fr.Consumer)))
	sh := (*reflect.SliceHeader)(unsafe.Pointer(&xsk.fillRing.Descs))
	sh.Data = uintptr(unsafe.Pointer(&fillRingSlice[0])) + uintptr(offsets.Fr.Desc)
	sh.Len = options.FillRingNumDescs
	sh.Cap = options.FillRingNumDescs

	completionRingSlice, err := syscall.Mmap(xsk.fd, unix.XDP_UMEM_PGOFF_COMPLETION_RING,
		int(offsets.Cr.Desc+uint64(options.CompletionRingNumDescs)*uint64(unsafe.Sizeof(uint64(0)))),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		xsk.Close()
		return nil, fmt.Errorf("syscall.Mmap XDP_UMEM_PGOFF_COMPLETION_RING failed: %v", err)
	}

	xsk.completionRing.Producer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&completionRingSlice[0])) + uintptr(offsets.Cr.Producer)))
	xsk.completionRing.Consumer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&completionRingSlice[0])) + uintptr(offsets.Cr.Consumer)))
	sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.completionRing.Descs))
	sh.Data = uintptr(unsafe.Pointer(&completionRingSlice[0])) + uintptr(offsets.Cr.Desc)
	sh.Len = options.CompletionRingNumDescs
	sh.Cap = options.CompletionRingNumDescs

	if rxRing {
		rxRingSlice, err := syscall.Mmap(xsk.fd, unix.XDP_PGOFF_RX_RING,
			int(offsets.Rx.Desc+uint64(options.RxRingNumDescs)*uint64(unsafe.Sizeof(Desc{}))),
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_SHARED|syscall.MAP_POPULATE)
		if err != nil {
			xsk.Close()
			return nil, fmt.Errorf("syscall.Mmap XDP_PGOFF_RX_RING failed: %v", err)
		}

		xsk.rxRing.Producer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&rxRingSlice[0])) + uintptr(offsets.Rx.Producer)))
		xsk.rxRing.Consumer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&rxRingSlice[0])) + uintptr(offsets.Rx.Consumer)))
		sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.rxRing.Descs))
		sh.Data = uintptr(unsafe.Pointer(&rxRingSlice[0])) + uintptr(offsets.Rx.Desc)
		sh.Len = options.RxRingNumDescs
		sh.Cap = options.RxRingNumDescs

		xsk.rxDescs = make([]Desc, 0, options.RxRingNumDescs)
	}

	if txRing {
		txRingSlice, err := syscall.Mmap(xsk.fd, unix.XDP_PGOFF_TX_RING,
			int(offsets.Tx.Desc+uint64(options.TxRingNumDescs)*uint64(unsafe.Sizeof(Desc{}))),
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_SHARED|syscall.MAP_POPULATE)
		if err != nil {
			xsk.Close()
			return nil, fmt.Errorf("syscall.Mmap XDP_PGOFF_TX_RING failed: %v", err)
		}

		xsk.txRing.Producer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&txRingSlice[0])) + uintptr(offsets.Tx.Producer)))
		xsk.txRing.Consumer = (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&txRingSlice[0])) + uintptr(offsets.Tx.Consumer)))
		sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.txRing.Descs))
		sh.Data = uintptr(unsafe.Pointer(&txRingSlice[0])) + uintptr(offsets.Tx.Desc)
		sh.Len = options.TxRingNumDescs
		sh.Cap = options.TxRingNumDescs
	}

	sa := unix.SockaddrXDP{
		Flags:   DefaultSocketFlags,
		Ifindex: uint32(ifindex),
		QueueID: uint32(QueueID),
	}
	if err = unix.Bind(xsk.fd, &sa); err != nil {
		xsk.Close()
		return nil, fmt.Errorf("%w: %v", ErrBindFailed, err)
	}

	xsk.freeRXDescs = make([]bool, options.NumFrames)
	for i := range xsk.freeRXDescs {
		xsk.freeRXDescs[i] = true
	}
	xsk.getRXDescs = make([]Desc, 0, options.FillRingNumDescs)
	xsk.txChan = make(chan Desc, options.FillRingNumDescs/2)
	xsk.manualTriggerTX = make(chan struct{}, 1)

	go xsk.txTransmit()
	go xsk.txWorker()
	return xsk, nil
}

// Fill submits the given descriptors to be filled (i.e. to receive frames into)
// it returns how many descriptors where actually put onto Fill ring queue.
// The descriptors can be acquired either by calling the GetRxDescs() method or
// by calling Receive() method.
func (xsk *Socket) Fill(descs []Desc) int {
	numFreeSlots := xsk.NumFreeFillSlots()
	if numFreeSlots < len(descs) {
		descs = descs[:numFreeSlots]
	}

	prod := *xsk.fillRing.Producer
	for _, desc := range descs {
		xsk.fillRing.Descs[prod&uint32(xsk.options.FillRingNumDescs-1)] = desc.Addr
		prod++
		xsk.freeRXDescs[desc.Addr/uint64(xsk.options.FrameSize)] = false
	}
	//fencer.SFence()
	*xsk.fillRing.Producer = prod

	xsk.numFilled += len(descs)

	return len(descs)
}

// Receive returns the descriptors which were filled, i.e. into which frames
// were received into.
func (xsk *Socket) Receive(num int) []Desc {
	numAvailable := xsk.NumReceived()
	if num > int(numAvailable) {
		num = int(numAvailable)
	}

	descs := xsk.rxDescs[:0]
	cons := *xsk.rxRing.Consumer
	//fencer.LFence()
	for i := 0; i < num; i++ {
		descs = append(descs, xsk.rxRing.Descs[cons&uint32(xsk.options.RxRingNumDescs-1)])
		cons++
		xsk.freeRXDescs[descs[i].Addr/uint64(xsk.options.FrameSize)] = true
	}
	//fencer.MFence()
	*xsk.rxRing.Consumer = cons

	xsk.numFilled -= len(descs)

	return descs
}

func (xsk *Socket) doTransmit() {
	var rc uintptr
	var errno syscall.Errno
	for {
		rc, _, errno = unix.Syscall6(syscall.SYS_SENDTO,
			uintptr(xsk.fd),
			0, 0,
			uintptr(unix.MSG_DONTWAIT),
			0, 0)
		if rc != 0 {
			switch errno {
			case unix.EINTR, unix.EAGAIN:
				// try again
				if numCompleted := xsk.NumCompleted(); numCompleted > 0 {
					xsk.Complete(numCompleted)
				}
			case unix.EBUSY: // "completed but not sent"
				return
			default:
				panic(fmt.Errorf("sendto failed with rc=%d and errno=%d", rc, errno))
			}
		} else {
			break
		}
	}
}

func (xsk *Socket) txTransmit() {
	t := time.NewTicker(xsk.options.TxStep)
	for {
		select {
		case <-t.C:
		case <-xsk.manualTriggerTX:
		}

		if *xsk.txRing.Producer == *xsk.txRing.Consumer {
			continue
		}
		xsk.doTransmit()
	}
}

func (xsk *Socket) txWorker() {
	for desc := range xsk.txChan {
		for uint32(xsk.options.TxRingNumDescs) == *xsk.txRing.Producer-*xsk.txRing.Consumer {
			xsk.manualTriggerTX <- struct{}{}
			slog.Warn("tx ring is full", slog.Int("fd", xsk.fd))
			time.Sleep(xsk.options.TxStep)
		}

		xsk.txRing.Descs[*xsk.txRing.Producer&uint32(xsk.options.TxRingNumDescs-1)] = desc
		*xsk.txRing.Producer++
	}
}

// FD returns the file descriptor associated with this xdp.Socket which can be
// used e.g. to do polling.
func (xsk *Socket) FD() int {
	return xsk.fd
}

// Poll blocks until kernel informs us that it has either received
// or completed (i.e. actually sent) some frames that were previously submitted
// using Fill() or Transmit() methods.
// The numReceived return value can be used as the argument for subsequent
// Receive() method call.
func (xsk *Socket) Poll(timeout int) (numReceived int, numCompleted int, err error) {
	var events int16
	if xsk.numFilled > 0 {
		events |= unix.POLLIN
	}
	if xsk.numTransmitted > 0 {
		events |= unix.POLLOUT
	}
	if events == 0 {
		return
	}

	var pfds [1]unix.PollFd
	pfds[0].Fd = int32(xsk.fd)
	pfds[0].Events = events
	for err = unix.EINTR; err == unix.EINTR; {
		_, err = unix.Poll(pfds[:], timeout)
	}
	if err != nil {
		return 0, 0, err
	}

	numReceived = xsk.NumReceived()
	if numCompleted = xsk.NumCompleted(); numCompleted > 0 {
		xsk.Complete(numCompleted)
	}

	return
}

// GetRxDescs returns up to n descriptors which are not currently in use.
// if rx is true, return desc in first half of umem, 2nd half otherwise
func (xsk *Socket) GetRxDescs(n int) []Desc {
	if n > cap(xsk.getRXDescs) {
		n = cap(xsk.getRXDescs)
	}

	descs := xsk.getRXDescs[:0]
	j := 0
	start := 0
	end := cap(xsk.getRXDescs)
	freeList := xsk.freeRXDescs
	for i := start; i < end && j < n; i++ {
		if freeList[i] == true {
			descs = append(descs, Desc{
				Addr: uint64(i) * uint64(xsk.options.FrameSize),
				Len:  uint32(xsk.options.FrameSize),
			})
			j++
		}
	}
	return descs
}

func (xsk *Socket) GetTxDesc() Desc {
	inc := xsk.txIndex.Add(1)
	offset := uint64(xsk.options.FillRingNumDescs) + (inc)%uint64(xsk.options.CompletionRingNumDescs)
	return Desc{
		Addr: offset * uint64(xsk.options.FrameSize),
		Len:  uint32(xsk.options.FrameSize),
	}
}

// GetFrame returns the buffer containing the frame corresponding to the given
// descriptor. The returned byte slice points to the actual buffer of the
// corresponding frame, so modiyfing this slice modifies the frame contents.
func (xsk *Socket) GetFrame(d Desc) []byte {
	return xsk.umem[d.Addr : d.Addr+uint64(d.Len)]
}

// Close closes and frees the resources allocated by the Socket.
func (xsk *Socket) Close() error {
	allErrors := []error{}
	var err error

	if xsk.txChan != nil {
		close(xsk.txChan)
	}
	if xsk.fd != -1 {
		if err = unix.Close(xsk.fd); err != nil {
			allErrors = append(allErrors, fmt.Errorf("failed to close XDP socket: %v", err))
		}
		xsk.fd = -1

		var sh *reflect.SliceHeader

		sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.completionRing.Descs))
		sh.Data = uintptr(0)
		sh.Len = 0
		sh.Cap = 0

		sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.txRing.Descs))
		sh.Data = uintptr(0)
		sh.Len = 0
		sh.Cap = 0

		sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.rxRing.Descs))
		sh.Data = uintptr(0)
		sh.Len = 0
		sh.Cap = 0

		sh = (*reflect.SliceHeader)(unsafe.Pointer(&xsk.fillRing.Descs))
		sh.Data = uintptr(0)
		sh.Len = 0
		sh.Cap = 0
	}

	if xsk.umem != nil {
		if err := syscall.Munmap(xsk.umem); err != nil {
			allErrors = append(allErrors, fmt.Errorf("failed to unmap the UMEM: %v", err))
		}
		xsk.umem = nil
	}

	if len(allErrors) > 0 {
		return allErrors[0]
	}

	return nil
}

// Complete consumes up to n descriptors from the Completion ring queue to
// which the kernel produces when it has actually transmitted a descriptor it
// got from Tx ring queue.
// You should use this method if you are doing polling on the xdp.Socket file
// descriptor yourself, rather than using the Poll() method.
func (xsk *Socket) Complete(n int) {
	*xsk.completionRing.Consumer += uint32(n)
}

// NumFreeFillSlots returns how many free slots are available on the Fill ring
// queue, i.e. the queue to which we produce descriptors which should be filled
// by the kernel with incoming frames.
func (xsk *Socket) NumFreeFillSlots() int {
	prod := *xsk.fillRing.Producer
	cons := *xsk.fillRing.Consumer
	max := uint32(xsk.options.FillRingNumDescs)

	n := max - (prod - cons)
	if n > max {
		n = max
	}

	return int(n)
}

// NumFreeTxSlots returns how many free slots are available on the Tx ring
// queue, i.e. the queue to which we produce descriptors which should be
// transmitted by the kernel to the wire.
func (xsk *Socket) NumFreeTxSlots() int {
	prod := *xsk.txRing.Producer
	cons := *xsk.txRing.Consumer
	max := uint32(xsk.options.TxRingNumDescs)

	n := max - (prod - cons)
	if n > max {
		n = max
	}

	return int(n)
}

// NumReceived returns how many descriptors are there on the Rx ring queue
// which were produced by the kernel and which we have not yet consumed.
func (xsk *Socket) NumReceived() int {
	prod := *xsk.rxRing.Producer
	cons := *xsk.rxRing.Consumer
	max := uint32(xsk.options.RxRingNumDescs)

	n := prod - cons
	if n > max {
		n = max
	}

	return int(n)
}

// NumCompleted returns how many descriptors are there on the Completion ring
// queue which were produced by the kernel and which we have not yet consumed.
func (xsk *Socket) NumCompleted() int {
	prod := *xsk.completionRing.Producer
	cons := *xsk.completionRing.Consumer
	max := uint32(xsk.options.CompletionRingNumDescs)

	n := prod - cons
	if n > max {
		n = max
	}

	return int(n)
}

// NumFilled returns how many descriptors are there on the Fill ring
// queue which have not yet been consumed by the kernel.
// This method is useful if you're polling the xdp.Socket file descriptor
// yourself, rather than using the Poll() method - if it returns a number
// greater than zero it means you should set the unix.POLLIN flag.
func (xsk *Socket) NumFilled() int {
	return xsk.numFilled
}

// NumTransmitted returns how many descriptors are there on the Tx ring queue
// which have not yet been consumed by the kernel.
// Note that even after the descriptors are consumed by the kernel from the Tx
// ring queue, it doesn't mean that they have actually been sent out on the
// wire, that can be assumed only after the descriptors have been produced by
// the kernel to the Completion ring queue.
// This method is useful if you're polling the xdp.Socket file descriptor
// yourself, rather than using the Poll() method - if it returns a number
// greater than zero it means you should set the unix.POLLOUT flag.
func (xsk *Socket) NumTransmitted() int {
	return xsk.numTransmitted
}

func (xsk *Socket) SendMsg(data []byte) error {
	desc := xsk.GetTxDesc()
	if desc.Addr == 0 {
		return errors.New("no free desc")
	}

	copy(xsk.GetFrame(desc), data)
	desc.Len = uint32(len(data))
	xsk.txChan <- desc

	return nil
}

// Stats returns various statistics for this XDP socket.
func (xsk *Socket) Stats() (Stats, error) {
	var stats Stats
	var size uint64

	stats.TxProd = uint64(*xsk.txRing.Producer)
	stats.TxCon = uint64(*xsk.txRing.Consumer)
	stats.CompletionProd = uint64(*xsk.completionRing.Producer)
	stats.CompletionCon = uint64(*xsk.completionRing.Consumer)
	stats.TxChan = len(xsk.txChan)
	size = uint64(unsafe.Sizeof(stats.KernelStats))
	rc, _, errno := unix.Syscall6(syscall.SYS_GETSOCKOPT,
		uintptr(xsk.fd),
		unix.SOL_XDP, unix.XDP_STATISTICS,
		uintptr(unsafe.Pointer(&stats.KernelStats)),
		uintptr(unsafe.Pointer(&size)), 0)
	if rc != 0 {
		return stats, fmt.Errorf("getsockopt XDP_STATISTICS failed with errno %d", errno)
	}
	return stats, nil
}
