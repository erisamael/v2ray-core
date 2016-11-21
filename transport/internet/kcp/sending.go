package kcp

import (
	"sync"
)

type SendingWindow struct {
	start uint32
	cap   uint32
	len   uint32
	last  uint32

	data  []DataSegment
	inuse []bool
	prev  []uint32
	next  []uint32

	totalInFlightSize uint32
	writer            SegmentWriter
	onPacketLoss      func(uint32)
}

func NewSendingWindow(size uint32, writer SegmentWriter, onPacketLoss func(uint32)) *SendingWindow {
	window := &SendingWindow{
		start:        0,
		cap:          size,
		len:          0,
		last:         0,
		data:         make([]DataSegment, size),
		prev:         make([]uint32, size),
		next:         make([]uint32, size),
		inuse:        make([]bool, size),
		writer:       writer,
		onPacketLoss: onPacketLoss,
	}
	return window
}

func (this *SendingWindow) Release() {
	if this == nil {
		return
	}
	for _, seg := range this.data {
		seg.Release()
	}
}

func (this *SendingWindow) Len() int {
	return int(this.len)
}

func (this *SendingWindow) IsEmpty() bool {
	return this.len == 0
}

func (this *SendingWindow) Size() uint32 {
	return this.cap
}

func (this *SendingWindow) IsFull() bool {
	return this.len == this.cap
}

func (this *SendingWindow) Push(number uint32, data []byte) {
	pos := (this.start + this.len) % this.cap
	this.data[pos].SetData(data)
	this.data[pos].Number = number
	this.data[pos].timeout = 0
	this.data[pos].transmit = 0
	this.inuse[pos] = true
	if this.len > 0 {
		this.next[this.last] = pos
		this.prev[pos] = this.last
	}
	this.last = pos
	this.len++
}

func (this *SendingWindow) FirstNumber() uint32 {
	return this.data[this.start].Number
}

func (this *SendingWindow) Clear(una uint32) {
	for !this.IsEmpty() && this.data[this.start].Number < una {
		this.Remove(0)
	}
}

func (this *SendingWindow) Remove(idx uint32) bool {
	if this.len == 0 {
		return false
	}

	pos := (this.start + idx) % this.cap
	if !this.inuse[pos] {
		return false
	}
	this.inuse[pos] = false
	this.totalInFlightSize--
	if pos == this.start && pos == this.last {
		this.len = 0
		this.start = 0
		this.last = 0
	} else if pos == this.start {
		delta := this.next[pos] - this.start
		if this.next[pos] < this.start {
			delta = this.next[pos] + this.cap - this.start
		}
		this.start = this.next[pos]
		this.len -= delta
	} else if pos == this.last {
		this.last = this.prev[pos]
	} else {
		this.next[this.prev[pos]] = this.next[pos]
		this.prev[this.next[pos]] = this.prev[pos]
	}
	return true
}

func (this *SendingWindow) HandleFastAck(number uint32, rto uint32) {
	if this.IsEmpty() {
		return
	}

	this.Visit(func(seg *DataSegment) bool {
		if number == seg.Number || number-seg.Number > 0x7FFFFFFF {
			return false
		}

		if seg.transmit > 0 && seg.timeout > rto/3 {
			seg.timeout -= rto / 3
		}
		return true
	})
}

func (this *SendingWindow) Visit(visitor func(seg *DataSegment) bool) {
	for i := this.start; ; i = this.next[i] {
		if !visitor(&this.data[i]) || i == this.last {
			break
		}
	}
}

func (this *SendingWindow) Flush(current uint32, rto uint32, maxInFlightSize uint32) {
	if this.IsEmpty() {
		return
	}

	var lost uint32
	var inFlightSize uint32

	this.Visit(func(segment *DataSegment) bool {
		if current-segment.timeout >= 0x7FFFFFFF {
			return true
		}
		if segment.transmit == 0 {
			// First time
			this.totalInFlightSize++
		} else {
			lost++
		}
		segment.timeout = current + rto

		segment.Timestamp = current
		segment.transmit++
		this.writer.Write(segment)
		inFlightSize++
		if inFlightSize >= maxInFlightSize {
			return false
		}
		return true
	})

	if this.onPacketLoss != nil && inFlightSize > 0 && this.totalInFlightSize != 0 {
		rate := lost * 100 / this.totalInFlightSize
		this.onPacketLoss(rate)
	}
}

type SendingWorker struct {
	sync.RWMutex
	conn                       *Connection
	window                     *SendingWindow
	firstUnacknowledged        uint32
	firstUnacknowledgedUpdated bool
	nextNumber                 uint32
	remoteNextNumber           uint32
	controlWindow              uint32
	fastResend                 uint32
}

func NewSendingWorker(kcp *Connection) *SendingWorker {
	worker := &SendingWorker{
		conn:             kcp,
		fastResend:       2,
		remoteNextNumber: 32,
		controlWindow:    kcp.Config.GetSendingInFlightSize(),
	}
	worker.window = NewSendingWindow(kcp.Config.GetSendingBufferSize(), worker, worker.OnPacketLoss)
	return worker
}

func (this *SendingWorker) Release() {
	this.window.Release()
}

func (this *SendingWorker) ProcessReceivingNext(nextNumber uint32) {
	this.Lock()
	defer this.Unlock()

	this.ProcessReceivingNextWithoutLock(nextNumber)
}

func (this *SendingWorker) ProcessReceivingNextWithoutLock(nextNumber uint32) {
	this.window.Clear(nextNumber)
	this.FindFirstUnacknowledged()
}

// Private: Visible for testing.
func (this *SendingWorker) FindFirstUnacknowledged() {
	v := this.firstUnacknowledged
	if !this.window.IsEmpty() {
		this.firstUnacknowledged = this.window.FirstNumber()
	} else {
		this.firstUnacknowledged = this.nextNumber
	}
	if v != this.firstUnacknowledged {
		this.firstUnacknowledgedUpdated = true
	}
}

// Private: Visible for testing.
func (this *SendingWorker) ProcessAck(number uint32) bool {
	// number < this.firstUnacknowledged || number >= this.nextNumber
	if number-this.firstUnacknowledged > 0x7FFFFFFF || number-this.nextNumber < 0x7FFFFFFF {
		return false
	}

	removed := this.window.Remove(number - this.firstUnacknowledged)
	if removed {
		this.FindFirstUnacknowledged()
	}
	return removed
}

func (this *SendingWorker) ProcessSegment(current uint32, seg *AckSegment, rto uint32) {
	defer seg.Release()

	this.Lock()
	defer this.Unlock()

	if this.remoteNextNumber < seg.ReceivingWindow {
		this.remoteNextNumber = seg.ReceivingWindow
	}
	this.ProcessReceivingNextWithoutLock(seg.ReceivingNext)

	var maxack uint32
	var maxackRemoved bool
	for i := 0; i < int(seg.Count); i++ {
		number := seg.NumberList[i]

		removed := this.ProcessAck(number)
		if maxack < number {
			maxack = number
			maxackRemoved = removed
		}
	}

	if maxackRemoved {
		this.window.HandleFastAck(maxack, rto)
		if current-seg.Timestamp < 10000 {
			this.conn.roundTrip.Update(current-seg.Timestamp, current)
		}
	}
}

func (this *SendingWorker) Push(b []byte) int {
	nBytes := 0
	this.Lock()
	defer this.Unlock()

	for len(b) > 0 && !this.window.IsFull() {
		var size int
		if len(b) > int(this.conn.mss) {
			size = int(this.conn.mss)
		} else {
			size = len(b)
		}
		this.window.Push(this.nextNumber, b[:size])
		this.nextNumber++
		b = b[size:]
		nBytes += size
	}
	return nBytes
}

// Private: Visible for testing.
func (this *SendingWorker) Write(seg Segment) {
	dataSeg := seg.(*DataSegment)

	dataSeg.Conv = this.conn.conv
	dataSeg.SendingNext = this.firstUnacknowledged
	dataSeg.Option = 0
	if this.conn.State() == StateReadyToClose {
		dataSeg.Option = SegmentOptionClose
	}

	this.conn.output.Write(dataSeg)
}

func (this *SendingWorker) OnPacketLoss(lossRate uint32) {
	if !this.conn.Config.Congestion || this.conn.roundTrip.Timeout() == 0 {
		return
	}

	if lossRate >= 15 {
		this.controlWindow = 3 * this.controlWindow / 4
	} else if lossRate <= 5 {
		this.controlWindow += this.controlWindow / 4
	}
	if this.controlWindow < 16 {
		this.controlWindow = 16
	}
	if this.controlWindow > 2*this.conn.Config.GetSendingInFlightSize() {
		this.controlWindow = 2 * this.conn.Config.GetSendingInFlightSize()
	}
}

func (this *SendingWorker) Flush(current uint32) {
	this.Lock()
	defer this.Unlock()

	cwnd := this.firstUnacknowledged + this.conn.Config.GetSendingInFlightSize()
	if cwnd > this.remoteNextNumber {
		cwnd = this.remoteNextNumber
	}
	if this.conn.Config.Congestion && cwnd > this.firstUnacknowledged+this.controlWindow {
		cwnd = this.firstUnacknowledged + this.controlWindow
	}

	if !this.window.IsEmpty() {
		this.window.Flush(current, this.conn.roundTrip.Timeout(), cwnd)
	} else if this.firstUnacknowledgedUpdated {
		this.conn.Ping(current, CommandPing)
	}

	this.firstUnacknowledgedUpdated = false
}

func (this *SendingWorker) CloseWrite() {
	this.Lock()
	defer this.Unlock()

	this.window.Clear(0xFFFFFFFF)
}

func (this *SendingWorker) IsEmpty() bool {
	this.RLock()
	defer this.RUnlock()

	return this.window.IsEmpty()
}

func (this *SendingWorker) UpdateNecessary() bool {
	return !this.IsEmpty()
}
