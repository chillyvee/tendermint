package p2p

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"time"

	wire "github.com/tendermint/go-wire"
	cmn "github.com/tendermint/tmlibs/common"
)

const (
	// PexChannel is a channel for PEX messages
	PexChannel = byte(0x00)

	// period to ensure peers connected
	defaultEnsurePeersPeriod = 30 * time.Second
	minNumOutboundPeers      = 10
	maxPexMessageSize        = 1048576 // 1MB

	// maximum pex messages one peer can send to us during `msgCountByPeerFlushInterval`
	defaultMaxMsgCountByPeer    = 1000
	msgCountByPeerFlushInterval = 1 * time.Hour
)

// PEXReactor handles PEX (peer exchange) and ensures that an
// adequate number of peers are connected to the switch.
//
// It uses `AddrBook` (address book) to store `NetAddress`es of the peers.
//
// ## Preventing abuse
//
// For now, it just limits the number of messages from one peer to
// `defaultMaxMsgCountByPeer` messages per `msgCountByPeerFlushInterval` (1000
// msg/hour).
//
// NOTE [2017-01-17]:
//   Limiting is fine for now. Maybe down the road we want to keep track of the
//   quality of peer messages so if peerA keeps telling us about peers we can't
//   connect to then maybe we should care less about peerA. But I don't think
//   that kind of complexity is priority right now.
type PEXReactor struct {
	BaseReactor

	book              *AddrBook
	config            *PEXReactorConfig
	ensurePeersPeriod time.Duration

	// tracks message count by peer, so we can prevent abuse
	msgCountByPeer    *cmn.CMap
	maxMsgCountByPeer uint16
}

// PEXReactorConfig holds reactor specific configuration data.
type PEXReactorConfig struct {
	// Seeds is a list of addresses reactor may use if it can't connect to peers
	// in the addrbook.
	Seeds []string
}

// NewPEXReactor creates new PEX reactor.
func NewPEXReactor(b *AddrBook, config *PEXReactorConfig) *PEXReactor {
	r := &PEXReactor{
		book:              b,
		config:            config,
		ensurePeersPeriod: defaultEnsurePeersPeriod,
		msgCountByPeer:    cmn.NewCMap(),
		maxMsgCountByPeer: defaultMaxMsgCountByPeer,
	}
	r.BaseReactor = *NewBaseReactor("PEXReactor", r)
	return r
}

// OnStart implements BaseService
func (r *PEXReactor) OnStart() error {
	if err := r.BaseReactor.OnStart(); err != nil {
		return err
	}
	err := r.book.Start()
	if err != nil && err != cmn.ErrAlreadyStarted {
		return err
	}
	go r.ensurePeersRoutine()
	go r.flushMsgCountByPeer()
	return nil
}

// OnStop implements BaseService
func (r *PEXReactor) OnStop() {
	r.BaseReactor.OnStop()
	r.book.Stop()
}

// GetChannels implements Reactor
func (r *PEXReactor) GetChannels() []*ChannelDescriptor {
	return []*ChannelDescriptor{
		{
			ID:                PexChannel,
			Priority:          1,
			SendQueueCapacity: 10,
		},
	}
}

// AddPeer implements Reactor by adding peer to the address book (if inbound)
// or by requesting more addresses (if outbound).
func (r *PEXReactor) AddPeer(p Peer) {
	if p.IsOutbound() {
		// For outbound peers, the address is already in the books.
		// Either it was added in DialPeersAsync or when we
		// received the peer's address in r.Receive
		if r.book.NeedMoreAddrs() {
			r.RequestPEX(p)
		}
	} else {
		// For inbound connections, the peer is its own source,
		// and its NodeInfo has already been validated
		addr := p.NodeInfo().NetAddress()
		r.book.AddAddress(addr, addr)
	}
}

// RemovePeer implements Reactor.
func (r *PEXReactor) RemovePeer(p Peer, reason interface{}) {
	// If we aren't keeping track of local temp data for each peer here, then we
	// don't have to do anything.
}

// Receive implements Reactor by handling incoming PEX messages.
func (r *PEXReactor) Receive(chID byte, src Peer, msgBytes []byte) {
	srcAddr := src.NodeInfo().NetAddress()
	r.IncrementMsgCountForPeer(srcAddr.ID)
	if r.ReachedMaxMsgCountForPeer(srcAddr.ID) {
		r.Logger.Error("Maximum number of messages reached for peer", "peer", srcAddr)
		// TODO remove src from peers?
		return
	}

	_, msg, err := DecodeMessage(msgBytes)
	if err != nil {
		r.Logger.Error("Error decoding message", "err", err)
		return
	}
	r.Logger.Debug("Received message", "src", src, "chId", chID, "msg", msg)

	switch msg := msg.(type) {
	case *pexRequestMessage:
		// src requested some peers.
		// NOTE: we might send an empty selection
		r.SendAddrs(src, r.book.GetSelection())
	case *pexAddrsMessage:
		// We received some peer addresses from src.
		// TODO: (We don't want to get spammed with bad peers)
		for _, netAddr := range msg.Addrs {
			if netAddr != nil {
				r.book.AddAddress(netAddr, srcAddr)
			}
		}
	default:
		r.Logger.Error(fmt.Sprintf("Unknown message type %v", reflect.TypeOf(msg)))
	}
}

// RequestPEX asks peer for more addresses.
func (r *PEXReactor) RequestPEX(p Peer) {
	p.Send(PexChannel, struct{ PexMessage }{&pexRequestMessage{}})
}

// SendAddrs sends addrs to the peer.
func (r *PEXReactor) SendAddrs(p Peer, netAddrs []*NetAddress) {
	p.Send(PexChannel, struct{ PexMessage }{&pexAddrsMessage{Addrs: netAddrs}})
}

// SetEnsurePeersPeriod sets period to ensure peers connected.
func (r *PEXReactor) SetEnsurePeersPeriod(d time.Duration) {
	r.ensurePeersPeriod = d
}

// SetMaxMsgCountByPeer sets maximum messages one peer can send to us during 'msgCountByPeerFlushInterval'.
func (r *PEXReactor) SetMaxMsgCountByPeer(v uint16) {
	r.maxMsgCountByPeer = v
}

// ReachedMaxMsgCountForPeer returns true if we received too many
// messages from peer with address `addr`.
// NOTE: assumes the value in the CMap is non-nil
func (r *PEXReactor) ReachedMaxMsgCountForPeer(peerID ID) bool {
	return r.msgCountByPeer.Get(string(peerID)).(uint16) >= r.maxMsgCountByPeer
}

// Increment or initialize the msg count for the peer in the CMap
func (r *PEXReactor) IncrementMsgCountForPeer(peerID ID) {
	var count uint16
	countI := r.msgCountByPeer.Get(string(peerID))
	if countI != nil {
		count = countI.(uint16)
	}
	count++
	r.msgCountByPeer.Set(string(peerID), count)
}

// Ensures that sufficient peers are connected. (continuous)
func (r *PEXReactor) ensurePeersRoutine() {
	// Randomize when routine starts
	ensurePeersPeriodMs := r.ensurePeersPeriod.Nanoseconds() / 1e6
	time.Sleep(time.Duration(rand.Int63n(ensurePeersPeriodMs)) * time.Millisecond)

	// fire once immediately.
	r.ensurePeers()

	// fire periodically
	ticker := time.NewTicker(r.ensurePeersPeriod)

	for {
		select {
		case <-ticker.C:
			r.ensurePeers()
		case <-r.Quit:
			ticker.Stop()
			return
		}
	}
}

// ensurePeers ensures that sufficient peers are connected. (once)
//
// Old bucket / New bucket are arbitrary categories to denote whether an
// address is vetted or not, and this needs to be determined over time via a
// heuristic that we haven't perfected yet, or, perhaps is manually edited by
// the node operator. It should not be used to compute what addresses are
// already connected or not.
//
// TODO Basically, we need to work harder on our good-peer/bad-peer marking.
// What we're currently doing in terms of marking good/bad peers is just a
// placeholder. It should not be the case that an address becomes old/vetted
// upon a single successful connection.
func (r *PEXReactor) ensurePeers() {
	numOutPeers, numInPeers, numDialing := r.Switch.NumPeers()
	numToDial := minNumOutboundPeers - (numOutPeers + numDialing)
	r.Logger.Info("Ensure peers", "numOutPeers", numOutPeers, "numDialing", numDialing, "numToDial", numToDial)
	if numToDial <= 0 {
		return
	}

	// bias to prefer more vetted peers when we have fewer connections.
	// not perfect, but somewhate ensures that we prioritize connecting to more-vetted
	// NOTE: range here is [10, 90]. Too high ?
	newBias := cmn.MinInt(numOutPeers, 8)*10 + 10

	toDial := make(map[ID]*NetAddress)
	// Try maxAttempts times to pick numToDial addresses to dial
	maxAttempts := numToDial * 3
	for i := 0; i < maxAttempts && len(toDial) < numToDial; i++ {
		try := r.book.PickAddress(newBias)
		if try == nil {
			continue
		}
		if _, selected := toDial[try.ID]; selected {
			continue
		}
		if dialling := r.Switch.IsDialing(try.ID); dialling {
			continue
		}
		if connected := r.Switch.Peers().Has(try.ID); connected {
			continue
		}
		r.Logger.Info("Will dial address", "addr", try)
		toDial[try.ID] = try
	}

	// Dial picked addresses
	for _, item := range toDial {
		go func(picked *NetAddress) {
			_, err := r.Switch.DialPeerWithAddress(picked, false)
			if err != nil {
				r.book.MarkAttempt(picked)
			}
		}(item)
	}

	// If we need more addresses, pick a random peer and ask for more.
	if r.book.NeedMoreAddrs() {
		peers := r.Switch.Peers().List()
		peersCount := len(peers)
		if peersCount > 0 {
			peer := peers[rand.Int()%peersCount] // nolint: gas
			r.Logger.Info("We need more addresses. Sending pexRequest to random peer", "peer", peer)
			r.RequestPEX(peer)
		}
	}

	// If we are not connected to nor dialing anybody, fallback to dialing seeds.
	if numOutPeers+numInPeers+numDialing+len(toDial) == 0 {
		r.Logger.Info("No addresses to dial nor connected peers. Will dial seeds", "seeds", r.config.Seeds)
		r.Switch.DialPeersAsync(r.book, r.config.Seeds, false)
	}
}

func (r *PEXReactor) flushMsgCountByPeer() {
	ticker := time.NewTicker(msgCountByPeerFlushInterval)

	for {
		select {
		case <-ticker.C:
			r.msgCountByPeer.Clear()
		case <-r.Quit:
			ticker.Stop()
			return
		}
	}
}

//-----------------------------------------------------------------------------
// Messages

const (
	msgTypeRequest = byte(0x01)
	msgTypeAddrs   = byte(0x02)
)

// PexMessage is a primary type for PEX messages. Underneath, it could contain
// either pexRequestMessage, or pexAddrsMessage messages.
type PexMessage interface{}

var _ = wire.RegisterInterface(
	struct{ PexMessage }{},
	wire.ConcreteType{&pexRequestMessage{}, msgTypeRequest},
	wire.ConcreteType{&pexAddrsMessage{}, msgTypeAddrs},
)

// DecodeMessage implements interface registered above.
func DecodeMessage(bz []byte) (msgType byte, msg PexMessage, err error) {
	msgType = bz[0]
	n := new(int)
	r := bytes.NewReader(bz)
	msg = wire.ReadBinary(struct{ PexMessage }{}, r, maxPexMessageSize, n, &err).(struct{ PexMessage }).PexMessage
	return
}

/*
A pexRequestMessage requests additional peer addresses.
*/
type pexRequestMessage struct {
}

func (m *pexRequestMessage) String() string {
	return "[pexRequest]"
}

/*
A message with announced peer addresses.
*/
type pexAddrsMessage struct {
	Addrs []*NetAddress
}

func (m *pexAddrsMessage) String() string {
	return fmt.Sprintf("[pexAddrs %v]", m.Addrs)
}
