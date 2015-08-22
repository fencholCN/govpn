/*
GoVPN -- simple secure free software virtual private network daemon
Copyright (C) 2014-2015 Sergey Matveev <stargrave@stargrave.org>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package govpn

import (
	"encoding/binary"
	"io"
	"time"

	"golang.org/x/crypto/poly1305"
	"golang.org/x/crypto/salsa20"
	"golang.org/x/crypto/xtea"
)

const (
	NonceSize       = 8
	NonceBucketSize = 128
	// S20BS is Salsa20's internal blocksize in bytes
	S20BS = 64
	// Maximal amount of bytes transfered with single key (4 GiB)
	MaxBytesPerKey int64 = 1 << 32
	// Size of packet's size mark in bytes
	PktSizeSize = 2
	// Heartbeat rate, relative to Timeout
	TimeoutHeartbeat = 4
)

type Peer struct {
	Addr string
	Id   *PeerId
	Conn io.Writer

	// Traffic behaviour
	NoiseEnable bool
	CPR         int
	CPRCycle    time.Duration `json:"-"`

	// Cryptography related
	Key          *[SSize]byte `json:"-"`
	NonceOur     uint64       `json:"-"`
	NonceRecv    uint64       `json:"-"`
	NonceCipher  *xtea.Cipher `json:"-"`
	nonceBucket0 map[uint64]struct{}
	nonceBucket1 map[uint64]struct{}
	nonceFound   bool
	nonceBucketN int32

	// Timers
	Timeout       time.Duration `json:"-"`
	Established   time.Time
	LastPing      time.Time
	LastSent      time.Time
	willSentCycle time.Time

	// This variables are initialized only once to relief GC
	buf       []byte
	tag       *[poly1305.TagSize]byte
	keyAuth   *[32]byte
	nonceRecv uint64
	frame     []byte
	nonce     []byte
	pktSize   uint64
	size      int
	now       time.Time

	// Statistics
	BytesIn         int64
	BytesOut        int64
	BytesPayloadIn  int64
	BytesPayloadOut int64
	FramesIn        int
	FramesOut       int
	FramesUnauth    int
	FramesDup       int
	HeartbeatRecv   int
	HeartbeatSent   int
}

func (p *Peer) String() string {
	return p.Id.String() + ":" + p.Addr
}

// Zero peer's memory state.
func (p *Peer) Zero() {
	sliceZero(p.Key[:])
	sliceZero(p.tag[:])
	sliceZero(p.keyAuth[:])
	sliceZero(p.buf)
	sliceZero(p.frame)
	sliceZero(p.nonce)
}

var (
	Emptiness = make([]byte, 1<<14)
	taps      = make(map[string]*TAP)
)

// Create TAP listening goroutine.
// This function takes required TAP interface name, opens it and allocates
// a buffer where all frame data will be written, channel where information
// about number of read bytes is sent to, synchronization channel (external
// processes tell that read buffer can be used again) and possible channel
// opening error.
func TAPListen(ifaceName string, timeout time.Duration, cpr int) (*TAP, chan []byte, chan struct{}, chan struct{}, error) {
	var tap *TAP
	var err error
	tap, exists := taps[ifaceName]
	if !exists {
		tap, err = NewTAP(ifaceName)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		taps[ifaceName] = tap
	}
	sink := make(chan []byte)
	sinkReady := make(chan struct{})
	sinkTerminate := make(chan struct{})
	sinkSkip := make(chan struct{})

	go func() {
		cprCycle := cprCycleCalculate(cpr)
		if cprCycle != time.Duration(0) {
			timeout = cprCycle
		} else {
			timeout = timeout / TimeoutHeartbeat
		}
		heartbeat := time.Tick(timeout)
		var pkt []byte
	ListenCycle:
		for {
			select {
			case <-sinkTerminate:
				break ListenCycle
			case <-heartbeat:
				go func() { sink <- make([]byte, 0) }()
				continue
			case <-sinkSkip:
			case <-sinkReady:
				tap.ready <- struct{}{}
				tap.synced = true
			}
		HeartbeatCatched:
			select {
			case <-heartbeat:
				go func() { sink <- make([]byte, 0) }()
				goto HeartbeatCatched
			case <-sinkTerminate:
				break ListenCycle
			case pkt = <-tap.sink:
				tap.synced = false
				sink <- pkt
			}
		}
		close(sink)
		close(sinkReady)
		close(sinkTerminate)
	}()
	if exists && tap.synced {
		sinkSkip <- struct{}{}
	} else {
		sinkReady <- struct{}{}
	}
	return tap, sink, sinkReady, sinkTerminate, nil
}

func newNonceCipher(key *[32]byte) *xtea.Cipher {
	nonceKey := make([]byte, 16)
	salsa20.XORKeyStream(
		nonceKey,
		make([]byte, 32),
		make([]byte, xtea.BlockSize),
		key,
	)
	ciph, err := xtea.NewCipher(nonceKey)
	if err != nil {
		panic(err)
	}
	return ciph
}

func cprCycleCalculate(rate int) time.Duration {
	if rate == 0 {
		return time.Duration(0)
	}
	return time.Second / time.Duration(rate*(1<<10)/MTU)
}

func newPeer(addr string, conn io.Writer, conf *PeerConf, nonce int, key *[SSize]byte) *Peer {
	now := time.Now()
	timeout := conf.Timeout
	cprCycle := cprCycleCalculate(conf.CPR)
	noiseEnable := conf.NoiseEnable
	if conf.CPR > 0 {
		noiseEnable = true
		timeout = cprCycle
	} else {
		timeout = timeout / TimeoutHeartbeat
	}
	peer := Peer{
		Addr:         addr,
		Conn:         conn,
		Timeout:      timeout,
		Established:  now,
		LastPing:     now,
		Id:           conf.Id,
		NoiseEnable:  noiseEnable,
		CPR:          conf.CPR,
		CPRCycle:     cprCycle,
		NonceOur:     uint64(nonce),
		NonceRecv:    uint64(0),
		nonceBucket0: make(map[uint64]struct{}, NonceBucketSize),
		nonceBucket1: make(map[uint64]struct{}, NonceBucketSize),
		Key:          key,
		NonceCipher:  newNonceCipher(key),
		buf:          make([]byte, MTU+S20BS),
		tag:          new([poly1305.TagSize]byte),
		keyAuth:      new([SSize]byte),
		nonce:        make([]byte, NonceSize),
	}
	return &peer
}

// Process incoming UDP packet.
// ConnListen'es synchronization channel used to tell him that he is
// free to receive new packets. Authenticated and decrypted packets
// will be written to the interface immediately (except heartbeat ones).
func (p *Peer) PktProcess(data []byte, tap io.Writer, ready chan struct{}) bool {
	p.size = len(data)
	copy(p.buf, Emptiness)
	copy(p.tag[:], data[p.size-poly1305.TagSize:])
	copy(p.buf[S20BS:], data[NonceSize:p.size-poly1305.TagSize])
	salsa20.XORKeyStream(
		p.buf[:S20BS+p.size-poly1305.TagSize],
		p.buf[:S20BS+p.size-poly1305.TagSize],
		data[:NonceSize],
		p.Key,
	)
	copy(p.keyAuth[:], p.buf[:SSize])
	if !poly1305.Verify(p.tag, data[:p.size-poly1305.TagSize], p.keyAuth) {
		ready <- struct{}{}
		p.FramesUnauth++
		return false
	}

	// Check if received nonce is known to us in either of two buckets.
	// If yes, then this is ignored duplicate.
	// Check from the oldest bucket, as in most cases this will result
	// in constant time check.
	// If Bucket0 is filled, then it becomes Bucket1.
	p.NonceCipher.Decrypt(p.buf, data[:NonceSize])
	ready <- struct{}{}
	p.nonceRecv, _ = binary.Uvarint(p.buf[:NonceSize])
	if _, p.nonceFound = p.nonceBucket1[p.NonceRecv]; p.nonceFound {
		p.FramesDup++
		return false
	}
	if _, p.nonceFound = p.nonceBucket0[p.NonceRecv]; p.nonceFound {
		p.FramesDup++
		return false
	}
	p.nonceBucket0[p.NonceRecv] = struct{}{}
	p.nonceBucketN++
	if p.nonceBucketN == NonceBucketSize {
		p.nonceBucket1 = p.nonceBucket0
		p.nonceBucket0 = make(map[uint64]struct{}, NonceBucketSize)
		p.nonceBucketN = 0
	}

	p.FramesIn++
	p.BytesIn += int64(p.size)
	p.LastPing = time.Now()
	p.NonceRecv = p.nonceRecv
	p.pktSize, _ = binary.Uvarint(p.buf[S20BS : S20BS+PktSizeSize])
	if p.pktSize == 0 {
		p.HeartbeatRecv++
		return true
	}
	p.frame = p.buf[S20BS+PktSizeSize : S20BS+PktSizeSize+p.pktSize]
	p.BytesPayloadIn += int64(p.pktSize)
	tap.Write(p.frame)
	return true
}

// Process incoming Ethernet packet.
// ready channel is TAPListen's synchronization channel used to tell him
// that he is free to receive new packets. Encrypted and authenticated
// packets will be sent to remote Peer side immediately.
func (p *Peer) EthProcess(data []byte, ready chan struct{}) {
	p.now = time.Now()
	p.size = len(data)
	// If this heartbeat is necessary
	if p.size == 0 && !p.LastSent.Add(p.Timeout).Before(p.now) {
		return
	}
	copy(p.buf, Emptiness)
	if p.size > 0 {
		copy(p.buf[S20BS+PktSizeSize:], data)
		ready <- struct{}{}
		binary.PutUvarint(p.buf[S20BS:S20BS+PktSizeSize], uint64(p.size))
		p.BytesPayloadOut += int64(p.size)
	} else {
		p.HeartbeatSent++
	}

	p.NonceOur += 2
	copy(p.nonce, Emptiness)
	binary.PutUvarint(p.nonce, p.NonceOur)
	p.NonceCipher.Encrypt(p.nonce, p.nonce)

	salsa20.XORKeyStream(p.buf, p.buf, p.nonce, p.Key)
	copy(p.buf[S20BS-NonceSize:S20BS], p.nonce)
	copy(p.keyAuth[:], p.buf[:SSize])
	if p.NoiseEnable {
		p.frame = p.buf[S20BS-NonceSize : S20BS+MTU-NonceSize-poly1305.TagSize]
	} else {
		p.frame = p.buf[S20BS-NonceSize : S20BS+PktSizeSize+p.size]
	}
	poly1305.Sum(p.tag, p.frame, p.keyAuth)

	p.BytesOut += int64(len(p.frame) + poly1305.TagSize)
	p.FramesOut++

	if p.CPRCycle != time.Duration(0) {
		p.willSentCycle = p.LastSent.Add(p.CPRCycle)
		if p.willSentCycle.After(p.now) {
			time.Sleep(p.willSentCycle.Sub(p.now))
			p.now = p.willSentCycle
		}
	}
	p.LastSent = p.now
	p.Conn.Write(append(p.frame, p.tag[:]...))
}
