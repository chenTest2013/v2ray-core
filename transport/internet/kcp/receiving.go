package kcp

import (
	"sync"

	"v2ray.com/core/common/alloc"
)

type ReceivingWindow struct {
	start uint32
	size  uint32
	list  []*DataSegment
}

func NewReceivingWindow(size uint32) *ReceivingWindow {
	return &ReceivingWindow{
		start: 0,
		size:  size,
		list:  make([]*DataSegment, size),
	}
}

func (v *ReceivingWindow) Size() uint32 {
	return v.size
}

func (v *ReceivingWindow) Position(idx uint32) uint32 {
	return (idx + v.start) % v.size
}

func (v *ReceivingWindow) Set(idx uint32, value *DataSegment) bool {
	pos := v.Position(idx)
	if v.list[pos] != nil {
		return false
	}
	v.list[pos] = value
	return true
}

func (v *ReceivingWindow) Remove(idx uint32) *DataSegment {
	pos := v.Position(idx)
	e := v.list[pos]
	v.list[pos] = nil
	return e
}

func (v *ReceivingWindow) RemoveFirst() *DataSegment {
	return v.Remove(0)
}

func (v *ReceivingWindow) Advance() {
	v.start++
	if v.start == v.size {
		v.start = 0
	}
}

type AckList struct {
	writer     SegmentWriter
	timestamps []uint32
	numbers    []uint32
	nextFlush  []uint32
}

func NewAckList(writer SegmentWriter) *AckList {
	return &AckList{
		writer:     writer,
		timestamps: make([]uint32, 0, 32),
		numbers:    make([]uint32, 0, 32),
		nextFlush:  make([]uint32, 0, 32),
	}
}

func (v *AckList) Add(number uint32, timestamp uint32) {
	v.timestamps = append(v.timestamps, timestamp)
	v.numbers = append(v.numbers, number)
	v.nextFlush = append(v.nextFlush, 0)
}

func (v *AckList) Clear(una uint32) {
	count := 0
	for i := 0; i < len(v.numbers); i++ {
		if v.numbers[i] < una {
			continue
		}
		if i != count {
			v.numbers[count] = v.numbers[i]
			v.timestamps[count] = v.timestamps[i]
			v.nextFlush[count] = v.nextFlush[i]
		}
		count++
	}
	if count < len(v.numbers) {
		v.numbers = v.numbers[:count]
		v.timestamps = v.timestamps[:count]
		v.nextFlush = v.nextFlush[:count]
	}
}

func (v *AckList) Flush(current uint32, rto uint32) {
	seg := NewAckSegment()
	for i := 0; i < len(v.numbers) && !seg.IsFull(); i++ {
		if v.nextFlush[i] > current {
			continue
		}
		seg.PutNumber(v.numbers[i])
		seg.PutTimestamp(v.timestamps[i])
		timeout := rto / 4
		if timeout < 20 {
			timeout = 20
		}
		v.nextFlush[i] = current + timeout
	}
	if seg.Count > 0 {
		v.writer.Write(seg)
		seg.Release()
	}
}

type ReceivingWorker struct {
	sync.RWMutex
	conn       *Connection
	leftOver   *alloc.Buffer
	window     *ReceivingWindow
	acklist    *AckList
	nextNumber uint32
	windowSize uint32
}

func NewReceivingWorker(kcp *Connection) *ReceivingWorker {
	worker := &ReceivingWorker{
		conn:       kcp,
		window:     NewReceivingWindow(kcp.Config.GetReceivingBufferSize()),
		windowSize: kcp.Config.GetReceivingInFlightSize(),
	}
	worker.acklist = NewAckList(worker)
	return worker
}

func (v *ReceivingWorker) Release() {
	v.leftOver.Release()
}

func (v *ReceivingWorker) ProcessSendingNext(number uint32) {
	v.Lock()
	defer v.Unlock()

	v.acklist.Clear(number)
}

func (v *ReceivingWorker) ProcessSegment(seg *DataSegment) {
	v.Lock()
	defer v.Unlock()

	number := seg.Number
	idx := number - v.nextNumber
	if idx >= v.windowSize {
		return
	}
	v.acklist.Clear(seg.SendingNext)
	v.acklist.Add(number, seg.Timestamp)

	if !v.window.Set(idx, seg) {
		seg.Release()
	}
}

func (v *ReceivingWorker) Read(b []byte) int {
	v.Lock()
	defer v.Unlock()

	total := 0
	if v.leftOver != nil {
		nBytes := copy(b, v.leftOver.Value)
		if nBytes < v.leftOver.Len() {
			v.leftOver.SliceFrom(nBytes)
			return nBytes
		}
		v.leftOver.Release()
		v.leftOver = nil
		total += nBytes
	}

	for total < len(b) {
		seg := v.window.RemoveFirst()
		if seg == nil {
			break
		}
		v.window.Advance()
		v.nextNumber++

		nBytes := copy(b[total:], seg.Data.Value)
		total += nBytes
		if nBytes < seg.Data.Len() {
			seg.Data.SliceFrom(nBytes)
			v.leftOver = seg.Data
			seg.Data = nil
			seg.Release()
			break
		}
		seg.Release()
	}
	return total
}

func (v *ReceivingWorker) Flush(current uint32) {
	v.Lock()
	defer v.Unlock()

	v.acklist.Flush(current, v.conn.roundTrip.Timeout())
}

func (v *ReceivingWorker) Write(seg Segment) {
	ackSeg := seg.(*AckSegment)
	ackSeg.Conv = v.conn.conv
	ackSeg.ReceivingNext = v.nextNumber
	ackSeg.ReceivingWindow = v.nextNumber + v.windowSize
	if v.conn.state == StateReadyToClose {
		ackSeg.Option = SegmentOptionClose
	}
	v.conn.output.Write(ackSeg)
}

func (v *ReceivingWorker) CloseRead() {
}

func (v *ReceivingWorker) UpdateNecessary() bool {
	return len(v.acklist.numbers) > 0
}
