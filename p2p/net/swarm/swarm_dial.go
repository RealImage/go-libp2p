package swarm

import (
	"errors"
	"fmt"
	"sync"
	"time"

	lgbl "github.com/ipfs/go-libp2p-loggables"
	peer "github.com/ipfs/go-libp2p-peer"
	conn "github.com/ipfs/go-libp2p/p2p/net/conn"
	addrutil "github.com/ipfs/go-libp2p/p2p/net/swarm/addr"
	ma "github.com/jbenet/go-multiaddr"
	context "golang.org/x/net/context"
)

// Diagram of dial sync:
//
//   many callers of Dial()   synched w.  dials many addrs       results to callers
//  ----------------------\    dialsync    use earliest            /--------------
//  -----------------------\              |----------\           /----------------
//  ------------------------>------------<-------     >---------<-----------------
//  -----------------------|              \----x                 \----------------
//  ----------------------|                \-----x                \---------------
//                                         any may fail          if no addr at end
//                                                             retry dialAttempt x

var (
	ErrDialBackoff = errors.New("dial backoff")
	ErrDialFailed  = errors.New("dial attempt failed")
	ErrDialToSelf  = errors.New("dial to self attempted")
)

// dialAttempts governs how many times a goroutine will try to dial a given peer.
// Note: this is down to one, as we have _too many dials_ atm. To add back in,
// add loop back in Dial(.)
const dialAttempts = 1

// number of concurrent outbound dials over transports that consume file descriptors
const concurrentFdDials = 160

// number of concurrent outbound dials to make per peer
const defaultPerPeerRateLimit = 8

// DialTimeout is the amount of time each dial attempt has. We can think about making
// this larger down the road, or putting more granular timeouts (i.e. within each
// subcomponent of Dial)
var DialTimeout time.Duration = time.Second * 10

// dialsync is a small object that helps manage ongoing dials.
// this way, if we receive many simultaneous dial requests, one
// can do its thing, while the rest wait.
//
// this interface is so would-be dialers can just:
//
//  for {
//  	c := findConnectionToPeer(peer)
//  	if c != nil {
//  		return c
//  	}
//
//  	// ok, no connections. should we dial?
//  	if ok, wait := dialsync.Lock(peer); !ok {
//  		<-wait // can optionally wait
//  		continue
//  	}
//  	defer dialsync.Unlock(peer)
//
//  	c := actuallyDial(peer)
//  	return c
//  }
//
type dialsync struct {
	// ongoing is a map of tickets for the current peers being dialed.
	// this way, we dont kick off N dials simultaneously.
	ongoing map[peer.ID]chan struct{}
	lock    sync.Mutex
}

// Lock governs the beginning of a dial attempt.
// If there are no ongoing dials, it returns true, and the client is now
// scheduled to dial. Every other goroutine that calls startDial -- with
//the same dst -- will block until client is done. The client MUST call
// ds.Unlock(p) when it is done, to unblock the other callers.
// The client is not reponsible for achieving a successful dial, only for
// reporting the end of the attempt (calling ds.Unlock(p)).
//
// see the example below `dialsync`
func (ds *dialsync) Lock(dst peer.ID) (bool, chan struct{}) {
	ds.lock.Lock()
	if ds.ongoing == nil { // init if not ready
		ds.ongoing = make(map[peer.ID]chan struct{})
	}
	wait, found := ds.ongoing[dst]
	if !found {
		ds.ongoing[dst] = make(chan struct{})
	}
	ds.lock.Unlock()

	if found {
		return false, wait
	}

	// ok! you're signed up to dial!
	return true, nil
}

// Unlock releases waiters to a dial attempt. see Lock.
// if Unlock(p) is called without calling Lock(p) first, Unlock panics.
func (ds *dialsync) Unlock(dst peer.ID) {
	ds.lock.Lock()
	wait, found := ds.ongoing[dst]
	if !found {
		panic("called dialDone with no ongoing dials to peer: " + dst.Pretty())
	}

	delete(ds.ongoing, dst) // remove ongoing dial
	close(wait)             // release everyone else
	ds.lock.Unlock()
}

// dialbackoff is a struct used to avoid over-dialing the same, dead peers.
// Whenever we totally time out on a peer (all three attempts), we add them
// to dialbackoff. Then, whenevers goroutines would _wait_ (dialsync), they
// check dialbackoff. If it's there, they don't wait and exit promptly with
// an error. (the single goroutine that is actually dialing continues to
// dial). If a dial is successful, the peer is removed from backoff.
// Example:
//
//  for {
//  	if ok, wait := dialsync.Lock(p); !ok {
//  		if backoff.Backoff(p) {
//  			return errDialFailed
//  		}
//  		<-wait
//  		continue
//  	}
//  	defer dialsync.Unlock(p)
//  	c, err := actuallyDial(p)
//  	if err != nil {
//  		dialbackoff.AddBackoff(p)
//  		continue
//  	}
//  	dialbackoff.Clear(p)
//  }
//

type dialbackoff struct {
	entries map[peer.ID]*backoffPeer
	lock    sync.RWMutex
}

type backoffPeer struct {
	tries int
	until time.Time
}

func (db *dialbackoff) init() {
	if db.entries == nil {
		db.entries = make(map[peer.ID]*backoffPeer)
	}
}

// Backoff returns whether the client should backoff from dialing
// peer p
func (db *dialbackoff) Backoff(p peer.ID) (backoff bool) {
	db.lock.Lock()
	defer db.lock.Unlock()
	db.init()
	bp, found := db.entries[p]
	if found && time.Now().Before(bp.until) {
		return true
	}

	return false
}

const baseBackoffTime = time.Second * 5
const maxBackoffTime = time.Minute * 5

// AddBackoff lets other nodes know that we've entered backoff with
// peer p, so dialers should not wait unnecessarily. We still will
// attempt to dial with one goroutine, in case we get through.
func (db *dialbackoff) AddBackoff(p peer.ID) {
	db.lock.Lock()
	defer db.lock.Unlock()
	db.init()
	bp, ok := db.entries[p]
	if !ok {
		db.entries[p] = &backoffPeer{
			tries: 1,
			until: time.Now().Add(baseBackoffTime),
		}
		return
	}

	expTimeAdd := time.Second * time.Duration(bp.tries*bp.tries)
	if expTimeAdd > maxBackoffTime {
		expTimeAdd = maxBackoffTime
	}
	bp.until = time.Now().Add(baseBackoffTime + expTimeAdd)
	bp.tries++
}

// Clear removes a backoff record. Clients should call this after a
// successful Dial.
func (db *dialbackoff) Clear(p peer.ID) {
	db.lock.Lock()
	defer db.lock.Unlock()
	db.init()
	delete(db.entries, p)
}

// Dial connects to a peer.
//
// The idea is that the client of Swarm does not need to know what network
// the connection will happen over. Swarm can use whichever it choses.
// This allows us to use various transport protocols, do NAT traversal/relay,
// etc. to achive connection.
func (s *Swarm) Dial(ctx context.Context, p peer.ID) (*Conn, error) {
	var logdial = lgbl.Dial("swarm", s.LocalPeer(), p, nil, nil)
	if p == s.local {
		log.Event(ctx, "swarmDialSelf", logdial)
		return nil, ErrDialToSelf
	}

	return s.gatedDialAttempt(ctx, p)
}

func (s *Swarm) bestConnectionToPeer(p peer.ID) *Conn {
	cs := s.ConnectionsToPeer(p)
	for _, conn := range cs {
		if conn != nil { // dump out the first one we find. (TODO pick better)
			return conn
		}
	}
	return nil
}

// gatedDialAttempt is an attempt to dial a node. It is gated by the swarm's
// dial synchronization systems: dialsync and dialbackoff.
func (s *Swarm) gatedDialAttempt(ctx context.Context, p peer.ID) (*Conn, error) {
	var logdial = lgbl.Dial("swarm", s.LocalPeer(), p, nil, nil)
	defer log.EventBegin(ctx, "swarmDialAttemptSync", logdial).Done()

	// check if we already have an open connection first
	conn := s.bestConnectionToPeer(p)
	if conn != nil {
		return conn, nil
	}

	// check if there's an ongoing dial to this peer
	if ok, wait := s.dsync.Lock(p); ok {
		defer s.dsync.Unlock(p)

		// if this peer has been backed off, lets get out of here
		if s.backf.Backoff(p) {
			log.Event(ctx, "swarmDialBackoff", logdial)
			return nil, ErrDialBackoff
		}

		// ok, we have been charged to dial! let's do it.
		// if it succeeds, dial will add the conn to the swarm itself.
		defer log.EventBegin(ctx, "swarmDialAttemptStart", logdial).Done()
		ctxT, cancel := context.WithTimeout(ctx, s.dialT)
		conn, err := s.dial(ctxT, p)
		cancel()
		log.Debugf("dial end %s", conn)
		if err != nil {
			log.Event(ctx, "swarmDialBackoffAdd", logdial)
			s.backf.AddBackoff(p) // let others know to backoff

			// ok, we failed. try again. (if loop is done, our error is output)
			return nil, fmt.Errorf("dial attempt failed: %s", err)
		}
		log.Event(ctx, "swarmDialBackoffClear", logdial)
		s.backf.Clear(p) // okay, no longer need to backoff
		return conn, nil

	} else {
		// we did not dial. we must wait for someone else to dial.

		// check whether we should backoff first...
		if s.backf.Backoff(p) {
			log.Event(ctx, "swarmDialBackoff", logdial)
			return nil, ErrDialBackoff
		}

		defer log.EventBegin(ctx, "swarmDialWait", logdial).Done()
		select {
		case <-wait: // wait for that other dial to finish.

			// see if it worked, OR we got an incoming dial in the meantime...
			conn := s.bestConnectionToPeer(p)
			if conn != nil {
				return conn, nil
			}
			return nil, ErrDialFailed
		case <-ctx.Done(): // or we may have to bail...
			return nil, ctx.Err()
		}
	}
}

// dial is the actual swarm's dial logic, gated by Dial.
func (s *Swarm) dial(ctx context.Context, p peer.ID) (*Conn, error) {
	var logdial = lgbl.Dial("swarm", s.LocalPeer(), p, nil, nil)
	if p == s.local {
		log.Event(ctx, "swarmDialDoDialSelf", logdial)
		return nil, ErrDialToSelf
	}
	defer log.EventBegin(ctx, "swarmDialDo", logdial).Done()
	logdial["dial"] = "failure" // start off with failure. set to "success" at the end.

	sk := s.peers.PrivKey(s.local)
	logdial["encrypted"] = (sk != nil) // log wether this will be an encrypted dial or not.
	if sk == nil {
		// fine for sk to be nil, just log.
		log.Debug("Dial not given PrivateKey, so WILL NOT SECURE conn.")
	}

	ila, _ := s.InterfaceListenAddresses()
	subtract_filter := addrutil.SubtractFilter(append(ila, s.peers.Addrs(s.local)...)...)

	// get live channel of addresses for peer, filtered by the given filters
	/*
		remoteAddrChan := s.peers.AddrsChan(ctx, p,
			addrutil.AddrUsableFilter,
			subtract_filter,
			s.Filters.AddrBlocked)
	*/

	//////
	/*
		This code is temporary, the peerstore can currently provide
		a channel as an interface for receiving addresses, but more thought
		needs to be put into the execution. For now, this allows us to use
		the improved rate limiter, while maintaining the outward behaviour
		that we previously had (halting a dial when we run out of addrs)
	*/
	paddrs := s.peers.Addrs(p)
	good_addrs := addrutil.FilterAddrs(paddrs,
		addrutil.AddrUsableFunc,
		subtract_filter,
		addrutil.FilterNeg(s.Filters.AddrBlocked),
	)
	remoteAddrChan := make(chan ma.Multiaddr, len(good_addrs))
	for _, a := range good_addrs {
		remoteAddrChan <- a
	}
	close(remoteAddrChan)
	/////////

	// try to get a connection to any addr
	connC, err := s.dialAddrs(ctx, p, remoteAddrChan)
	if err != nil {
		logdial["error"] = err
		return nil, err
	}
	logdial["netconn"] = lgbl.NetConn(connC)

	// ok try to setup the new connection.
	defer log.EventBegin(ctx, "swarmDialDoSetup", logdial, lgbl.NetConn(connC)).Done()
	swarmC, err := dialConnSetup(ctx, s, connC)
	if err != nil {
		logdial["error"] = err
		connC.Close() // close the connection. didn't work out :(
		return nil, err
	}

	logdial["dial"] = "success"
	return swarmC, nil
}

func (s *Swarm) dialAddrs(ctx context.Context, p peer.ID, remoteAddrs <-chan ma.Multiaddr) (conn.Conn, error) {
	log.Debugf("%s swarm dialing %s %s", s.local, p, remoteAddrs)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // cancel work when we exit func

	// use a single response type instead of errs and conns, reduces complexity *a ton*
	respch := make(chan dialResult)

	defaultDialFail := fmt.Errorf("failed to dial %s (default failure)", p)
	exitErr := defaultDialFail

	var active int
	for {
		select {
		case addr, ok := <-remoteAddrs:
			if !ok {
				remoteAddrs = nil
				if active == 0 {
					return nil, exitErr
				}
				continue
			}

			s.limitedDial(ctx, p, addr, respch)
			active++
		case <-ctx.Done():
			if exitErr == defaultDialFail {
				exitErr = ctx.Err()
			}
			return nil, exitErr
		case resp := <-respch:
			active--
			if resp.Err != nil {
				log.Info("got error on dial: ", resp.Err)
				// Errors are normal, lots of dials will fail
				exitErr = resp.Err

				if remoteAddrs == nil && active == 0 {
					return nil, exitErr
				}
			} else if resp.Conn != nil {
				return resp.Conn, nil
			}
		}
	}
}

// limitedDial will start a dial to the given peer when
// it is able, respecting the various different types of rate
// limiting that occur without using extra goroutines per addr
func (s *Swarm) limitedDial(ctx context.Context, p peer.ID, a ma.Multiaddr, resp chan dialResult) {
	s.limiter.AddDialJob(&dialJob{
		addr: a,
		peer: p,
		resp: resp,
		ctx:  ctx,
	})
}

func (s *Swarm) dialAddr(ctx context.Context, p peer.ID, addr ma.Multiaddr) (conn.Conn, error) {
	log.Debugf("%s swarm dialing %s %s", s.local, p, addr)

	connC, err := s.dialer.Dial(ctx, addr, p)
	if err != nil {
		return nil, fmt.Errorf("%s --> %s dial attempt failed: %s", s.local, p, err)
	}

	// if the connection is not to whom we thought it would be...
	remotep := connC.RemotePeer()
	if remotep != p {
		connC.Close()
		return nil, fmt.Errorf("misdial to %s through %s (got %s)", p, addr, remotep)
	}

	// if the connection is to ourselves...
	// this can happen TONS when Loopback addrs are advertized.
	// (this should be caught by two checks above, but let's just make sure.)
	if remotep == s.local {
		connC.Close()
		return nil, fmt.Errorf("misdial to %s through %s (got self)", p, addr)
	}

	// success! we got one!
	return connC, nil
}

// dialConnSetup is the setup logic for a connection from the dial side. it
// needs to add the Conn to the StreamSwarm, then run newConnSetup
func dialConnSetup(ctx context.Context, s *Swarm, connC conn.Conn) (*Conn, error) {

	psC, err := s.swarm.AddConn(connC)
	if err != nil {
		// connC is closed by caller if we fail.
		return nil, fmt.Errorf("failed to add conn to ps.Swarm: %s", err)
	}

	// ok try to setup the new connection. (newConnSetup will add to group)
	swarmC, err := s.newConnSetup(ctx, psC)
	if err != nil {
		psC.Close() // we need to make sure psC is Closed.
		return nil, err
	}

	return swarmC, err
}
