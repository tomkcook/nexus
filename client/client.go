/*
Package client provides a WAMP client implementation that is interoperable with
any standard WAMP router and is capable of using all of the advanced profile
features supported by the nexus WAMP router.

*/
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/nexus/stdlog"
	"github.com/gammazero/nexus/transport"
	"github.com/gammazero/nexus/transport/serialize"
	"github.com/gammazero/nexus/wamp"
)

const (
	helloAuthmethods = "authmethods"
	helloRoles       = "roles"
)

// Deprecated: replaced by Config
//
// ClientConfig is a type alias for the deprecated ClientConfig.
type ClientConfig = Config

// Config configures a client with everything needed to begin a session
// with a WAMP router.
type Config struct {
	// Realm is the URI of the realm the client will join.
	Realm string

	// HelloDetails contains details about the client.  The client provides the
	// roles, unless already supplied by the user.
	HelloDetails wamp.Dict

	// AuthHandlers is a map of authmethod to AuthFunc.  All authmethod keys
	// from this map are automatically added to HelloDetails["authmethods"]
	AuthHandlers map[string]AuthFunc

	// ResponseTimeout specifies the amount of time that the client will block
	// waiting for a response from the router.  A value of 0 uses the default.
	ResponseTimeout time.Duration

	// Enable debug logging for client.
	Debug bool

	// Set to JSON or MSGPACK.  Default (zero-value) is JSON.
	Serialization serialize.Serialization

	// Provide a tls.Config to connect the client using TLS.  The zero
	// configuration specifies using defaults.  A nil tls.Config means do not
	// use TLS.
	TlsCfg *tls.Config

	// Supplies alternate Dial function for the websocket dialer.
	// See https://godoc.org/github.com/gorilla/websocket#Dialer
	Dial transport.DialFunc

	// Client receive limit for use with RawSocket transport.
	// If recvLimit is > 0, then the client will not receive messages with size
	// larger than the nearest power of 2 greater than or equal to recvLimit.
	// If recvLimit is <= 0, then the default of 16M is used.
	RecvLimit int

	// Logger for client to use.  If not set, client logs to os.Stderr.
	Logger stdlog.StdLog

	// Websocket transport configuration.
	WsCfg transport.WebsocketConfig
}

// Define serialization consts in client package so that client code does not
// need to import the serialize package to get the consts.
const (
	JSON    = serialize.JSON
	MSGPACK = serialize.MSGPACK
	CBOR    = serialize.CBOR
)

// Features supported by nexus client.
var clientRoles = wamp.Dict{
	"publisher": wamp.Dict{
		"features": wamp.Dict{
			"subscriber_blackwhite_listing": true,
			"publisher_exclusion":           true,
		},
	},
	"subscriber": wamp.Dict{
		"features": wamp.Dict{
			"pattern_based_subscription": true,
			"publisher_identification":   true,
		},
	},
	"callee": wamp.Dict{
		"features": wamp.Dict{
			"pattern_based_registration": true,
			"shared_registration":        true,
			"call_canceling":             true,
			"call_timeout":               true,
			"caller_identification":      true,
			"progressive_call_results":   true,
		},
	},
	"caller": wamp.Dict{
		"features": wamp.Dict{
			"call_canceling":           true,
			"call_timeout":             true,
			"caller_identification":    true,
			"progressive_call_results": true,
		},
	},
}

// InvokeResult represents the result of invoking a procedure.
type InvokeResult struct {
	Args   wamp.List
	Kwargs wamp.Dict
	Err    wamp.URI
}

// A Client routes messages to/from a WAMP router.
type Client struct {
	sess wamp.Session

	responseTimeout time.Duration
	awaitingReply   map[wamp.ID]chan wamp.Message
	replyLock       sync.Mutex
	authHandlers    map[string]AuthFunc

	eventHandlers map[wamp.ID]EventHandler
	topicSubID    map[string]wamp.ID

	invHandlers    map[wamp.ID]InvocationHandler
	nameProcID     map[string]wamp.ID
	invHandlerKill map[wamp.ID]context.CancelFunc
	progGate       map[context.Context]wamp.ID
	progGateLock   sync.Mutex

	actionChan chan func()

	stopping          chan struct{}
	activeInvHandlers sync.WaitGroup

	log   stdlog.StdLog
	debug bool

	closed        int32
	done          chan struct{}
	routerGoodbye *wamp.Goodbye
	idGen         *wamp.SyncIDGen
}

// NewClient takes a connected Peer, joins the realm specified in cfg, and if
// successful, returns a new client.
//
// NOTE: This method is exported for clients that use a Peer implementation not
// provided with the nexus package.  Generally, clients are created using
// ConnectNet() or ConnectLocal().
func NewClient(p wamp.Peer, cfg Config) (*Client, error) {
	if cfg.ResponseTimeout == 0 {
		cfg.ResponseTimeout = defaultResponseTimeout
	}

	welcome, err := joinRealm(p, cfg)
	if err != nil {
		p.Close()
		return nil, err
	}

	c := &Client{
		sess: wamp.Session{
			Peer:    p,
			ID:      welcome.ID,
			Details: welcome.Details,
		},

		responseTimeout: cfg.ResponseTimeout,
		awaitingReply:   map[wamp.ID]chan wamp.Message{},

		eventHandlers: map[wamp.ID]EventHandler{},
		topicSubID:    map[string]wamp.ID{},

		invHandlers:    map[wamp.ID]InvocationHandler{},
		nameProcID:     map[string]wamp.ID{},
		invHandlerKill: map[wamp.ID]context.CancelFunc{},
		progGate:       map[context.Context]wamp.ID{},

		actionChan: make(chan func()),
		stopping:   make(chan struct{}),
		done:       make(chan struct{}),

		log:   cfg.Logger,
		debug: cfg.Debug,
		idGen: new(wamp.SyncIDGen),
	}
	go c.run() // start the core goroutine
	return c, nil
}

// Done returns a channel that signals when the client is no longer connected
// to a router and has shutdown.
func (c *Client) Done() <-chan struct{} { return c.done }

// ID returns the client's session ID which is assigned after attaching to a
// router and joining a realm.
func (c *Client) ID() wamp.ID { return c.sess.ID }

// Logger returns the clients logger that was provided by Config when the
// client was created, or the stdout logger if one was not provided in Config.
func (c *Client) Logger() stdlog.StdLog { return c.log }

// AuthFunc takes the CHALLENGE message and returns the signature string and
// any WELCOME message details.  If the signature is accepted, the details are
// used to populate the welcome message, as well as the session attributes.
//
// In response to a CHALLENGE message, the Client MUST send an AUTHENTICATE
// message.  Therefore, AuthFunc does not return an error.  If an error is
// encountered within AuthFunc, then an empty signature should be returned
// since the client cannot give a valid signature response.
//
// This is used in the AuthHandler map, in a Config, and is used when the
// client joins a realm.
type AuthFunc func(challenge *wamp.Challenge) (signature string, details wamp.Dict)

// RealmDetails returns the realm information received in the WELCOME message.
func (c *Client) RealmDetails() wamp.Dict { return c.sess.Details }

// HasFeature returns true if the session has the specified feature for the
// specified role.
func (c *Client) HasFeature(role, feature string) bool {
	return c.sess.HasFeature(role, feature)
}

// EventHandler is a function that handles a publish event.
type EventHandler func(args wamp.List, kwargs, details wamp.Dict)

// Subscribe subscribes the client to the specified topic or topic pattern.
//
// The specified EventHandler is registered to be called every time an event is
// received for the topic.  The subscription can specify an exact event URI to
// match, or it can specify a URI pattern to match multiple events for the same
// handler by specifying the pattern type in options.
//
// Subscribe Options
//
// To request a pattern-based subscription set:
//   options["match"] = "prefix" or "wildcard"
//
// NOTE: Use consts defined in wamp/options.go instead of raw strings.
func (c *Client) Subscribe(topic string, fn EventHandler, options wamp.Dict) error {
	if options == nil {
		options = wamp.Dict{}
	}
	id := c.idGen.Next()
	c.expectReply(id)
	c.sess.Send(&wamp.Subscribe{
		Request: id,
		Options: options,
		Topic:   wamp.URI(topic),
	})

	// Wait to receive SUBSCRIBED message.
	msg, err := c.waitForReply(id)
	if err != nil {
		return err
	}
	switch msg := msg.(type) {
	case *wamp.Subscribed:
		// Register the event handler for this subscription.
		return c.runAction(func() {
			c.eventHandlers[msg.Subscription] = fn
			c.topicSubID[topic] = msg.Subscription
		})
	case *wamp.Error:
		return fmt.Errorf("subscribing to topic '%v': %s", topic,
			wampErrorString(msg))
	default:
		return unexpectedMsgError(msg, wamp.SUBSCRIBED)
	}
}

// SubscriptionID returns the subscription ID for the specified topic.  If the
// client does not have an active subscription to the topic, then returns false
// for second boolean return value.
func (c *Client) SubscriptionID(topic string) (subID wamp.ID, ok bool) {
	c.runAction(func() {
		subID, ok = c.topicSubID[topic]
	})
	return
}

// Unsubscribe removes the registered EventHandler from the topic.
func (c *Client) Unsubscribe(topic string) error {
	var subID wamp.ID
	var err error
	runErr := c.runAction(func() {
		var ok bool
		subID, ok = c.topicSubID[topic]
		if !ok {
			err = errors.New("not subscribed to: " + topic)
		} else {
			// Delete the subscription anyway, regardless of whether or not the
			// the router succeeds or fails to unsubscribe.  If the client
			// called Unsubscribe() then it has no interest in receiving any
			// more events for the topic, and may expect any.
			delete(c.topicSubID, topic)
			delete(c.eventHandlers, subID)
		}
	})
	if runErr != nil {
		return runErr
	}
	if err != nil {
		return err
	}

	id := c.idGen.Next()
	c.expectReply(id)
	c.sess.Send(&wamp.Unsubscribe{
		Request:      id,
		Subscription: subID,
	})

	// Wait to receive UNSUBSCRIBED message.
	msg, err := c.waitForReply(id)
	if err != nil {
		return err
	}
	switch msg := msg.(type) {
	case *wamp.Unsubscribed:
		// Already deleted the event handler for the topic.
		return nil
	case *wamp.Error:
		return fmt.Errorf("unsubscribing to '%s': %s", topic,
			wampErrorString(msg))
	}
	return unexpectedMsgError(msg, wamp.UNSUBSCRIBED)
}

// Publish publishes an EVENT to all subscribed clients.
//
// Publish Options
//
// To receive a PUBLISHED response set:
//   options["acknowledge"] = true
//
// To request subscriber blacklisting by subscriber, authid, or authrole, set:
//   options["exclude"] = [subscriberID, ...]
//   options["exclude_authid"] = ["authid", ..]
//   options["exclude_authrole"] = ["authrole", ..]
//
// To request subscriber whitelisting by subscriber, authid, or authrole, set:
//   options["eligible"] = [subscriberID, ...]
//   options["eligible_authid"] = ["authid", ..]
//   options["eligible_authrole"] = ["authrole", ..]
//
// When connecting to a nexus router, blacklisting and whitelisting can be used
// with any attribute assigned to the subscriber session, by setting:
//   options["exclude_xxx"] = [val1, val2, ..]
// and
//   options["eligible_xxx"] = [val1, val2, ..]
// where xxx is the name of any session attribute, typically supplied with the
// HELLO message.
//
// To receive a published event, the subscriber session must not have any
// values that appear in a blacklist, and must have a value from each
// whitelist.
//
// To request that this publisher's identity is disclosed to subscribers, set:
//   options["disclose_me"] = true
//
// NOTE: Use consts defined in wamp/options.go instead of raw strings.
func (c *Client) Publish(topic string, options wamp.Dict, args wamp.List, kwargs wamp.Dict) error {
	if options == nil {
		options = make(wamp.Dict)
	}

	// Check if the client is asking for a PUBLISHED response.
	pubAck, _ := options[wamp.OptAcknowledge].(bool)

	id := c.idGen.Next()
	if pubAck {
		c.expectReply(id)
	}
	c.sess.Send(&wamp.Publish{
		Request:     id,
		Options:     options,
		Topic:       wamp.URI(topic),
		Arguments:   args,
		ArgumentsKw: kwargs,
	})

	if !pubAck {
		return nil
	}

	// Wait to receive PUBLISHED message.
	msg, err := c.waitForReply(id)
	if err != nil {
		return err
	}
	switch msg := msg.(type) {
	case *wamp.Published:
	case *wamp.Error:
		return fmt.Errorf("waiting for published message: %s", wampErrorString(msg))
	default:
		return unexpectedMsgError(msg, wamp.PUBLISHED)
	}
	return nil
}

// InvocationHandler handles a remote procedure call.
//
// The Context is used to signal that the router issued an INTERRUPT request to
// cancel the call-in-progress.  The client application can use this to
// abandon what it is doing, if it chooses to pay attention to ctx.Done().
//
// If the callee wishes to send progressive results, and the caller is willing
// to receive them, SendProgress() may be called from within an
// InvocationHandler for each progressive result to send to the caller.  It is
// not required that the handler send any progressive results.
type InvocationHandler func(context.Context, wamp.List, wamp.Dict, wamp.Dict) (result *InvokeResult)

// Register registers the client to handle invocations of the specified
// procedure.  The InvocationHandler is set to be called for each procedure
// call received.
//
// Register Options
//
// To request a pattern-based registration set:
//   options["match"] = "prefix" or "wildcard"
//
// To request a shared registration pattern set:
//   options["invoke"] = "single", "roundrobin", "random", "first", "last"
//
// To request that caller identification is disclosed to this callee, set:
//   options["disclose_caller"] = true
//
// NOTE: Use consts defined in wamp/options.go instead of raw strings.
func (c *Client) Register(procedure string, fn InvocationHandler, options wamp.Dict) error {
	id := c.idGen.Next()
	c.expectReply(id)
	if options == nil {
		options = wamp.Dict{}
	}
	c.sess.Send(&wamp.Register{
		Request:   id,
		Options:   options,
		Procedure: wamp.URI(procedure),
	})

	// Wait to receive REGISTERED message.
	msg, err := c.waitForReply(id)
	if err != nil {
		return err
	}
	switch msg := msg.(type) {
	case *wamp.Registered:
		// Register the event handler for this registration.
		return c.runAction(func() {
			c.invHandlers[msg.Registration] = fn
			c.nameProcID[procedure] = msg.Registration

			if c.debug {
				c.log.Println("Registered", procedure, "as registration",
					msg.Registration)
			}
		})
	case *wamp.Error:
		return fmt.Errorf("registering procedure '%v': %s", procedure,
			wampErrorString(msg))
	default:
		return unexpectedMsgError(msg, wamp.REGISTERED)
	}
}

// runAction runs the action on the main run() goroutine blocking waiting for a
// response
func (c *Client) runAction(action func()) error {
	sync := make(chan struct{})
	select {
	case c.actionChan <- func() {
		action()
		close(sync)
	}:
	case <-c.done:
		if c.debug {
			c.log.Println("Action cancelled client is closed")
		}
		return errors.New("client closed")
	}
	<-sync
	return nil
}

// RegistrationID returns the registration ID for the specified procedure.  If
// the client is not registered for the procedure, then returns false for
// second boolean return value.
func (c *Client) RegistrationID(procedure string) (regID wamp.ID, ok bool) {
	c.runAction(func() {
		regID, ok = c.nameProcID[procedure]
	})
	return
}

// Unregister removes the registration of a procedure from the router.
func (c *Client) Unregister(procedure string) error {
	var procID wamp.ID
	var err error
	runErr := c.runAction(func() {
		var ok bool
		procID, ok = c.nameProcID[procedure]
		if !ok {
			err = errors.New("not registered to handle procedure " + procedure)
		} else {
			// Delete the registration anyway, regardless of whether or not the
			// the router succeeds or fails to unregister.  If the client
			// called Unregister() then it has no interest in receiving any
			// more invocations for the procedure, and may not expect any.
			delete(c.nameProcID, procedure)
			delete(c.invHandlers, procID)
		}
	})
	if runErr != nil {
		return runErr
	}
	if err != nil {
		return err
	}

	id := c.idGen.Next()
	c.expectReply(id)
	c.sess.Send(&wamp.Unregister{
		Request:      id,
		Registration: procID,
	})

	// Wait to receive UNREGISTERED message.
	msg, err := c.waitForReply(id)
	if err != nil {
		return err
	}
	switch msg := msg.(type) {
	case *wamp.Unregistered:
		// Already deleted the invocation handler for the procedure.
	case *wamp.Error:
		return fmt.Errorf("unregistering procedure '%s': %v", procedure,
			wampErrorString(msg))
	default:
		return unexpectedMsgError(msg, wamp.UNREGISTERED)
	}
	return nil
}

// Call calls the procedure corresponding to the given URI.
//
// If an ERROR message is received from the router, the error value returned
// can be type asserted to RPCError to provide access to the returned ERROR
// message.  This may be necessary for the client application to process error
// data from the RPC invocation.
//
// Call Canceling
//
// The provided Context allows the caller to cancel a call, or to set a
// deadline that cancels the call when the deadline expires.  There is no
// separate Cancel() API to do this.  If the call is canceled before a result
// is received, then a CANCEL message is sent to the router to cancel the call
// according to the specified mode.
//
// If the context is canceled or times out, then error returned will not be a
// RPCError.  This allows the caller to distinquish between cancellation
// initiated by the client (by canceling context), and cancellation initialed
// elsewhere.
//
// Cancel Mode
//
// cancelMode must be one of the following: "kill", "killnowait', "skip".
// Setting to "" specifies using the default value: "killnowait".  cancelMode
// is an option for a CANCEL message, not for the CALL message, which is why it
// is specified as a parameter to the Call() API, and not a message option for
// CALL.
//
// Cancele Mode Behavior
//
// "skip": The pending call is canceled and ERROR is sent immediately back to
// the caller.  No INTERRUPT is sent to the callee and the result is discarded
// when received.
//
// "kill": INTERRUPT is sent to the client, but ERROR is not returned to the
// caller until after the callee has responded to the canceled call.  In this
// case the caller may receive RESULT or ERROR depending whether the callee
// finishes processing the invocation or the interrupt first.
//
// "killnowait": The pending call is canceled and ERROR is sent immediately
// back to the caller.  INTERRUPT is sent to the callee and any response to the
// invocation or interrupt from the callee is discarded when received.
//
// If the callee does not support call canceling, then behavior is "skip".
//
// Call Timeout
//
// The nexus router also supports call timeout.  If a timeout is provided in
// the options, and the callee supports call timeout, then the timeout value is
// passed to the callee so that the invocation can be canceled by the callee
// if the timeout is reached before a response is returned.  This is the
// behavior implemented by the nexus client in the callee role.
//
// To request a remote call timeout, specify a timeout in milliseconds:
//   options["timeout"] = 30000
//
// Caller Identification
//
// A caller may request the disclosure of its identity (its WAMP session ID) to
// callees, if allowed by the dealer.
//
// To request that this caller's identity disclosed to callees, set:
//   options["disclose_me"] = true
//
// NOTE: Use consts defined in wamp/options.go instead of raw strings.
//
// Progressive Call Results
//
// To request progressive call results, use the CallProgress function.
func (c *Client) Call(ctx context.Context, procedure string, options wamp.Dict, args wamp.List, kwargs wamp.Dict, cancelMode string) (*wamp.Result, error) {
	return c.CallProgress(ctx, procedure, options, args, kwargs, cancelMode, nil)
}

// ProgressCallback is a type of function that is registered to asynchronously
// handle progressive results during the a call to CallProgress().
type ProgressCallback func(*wamp.Result)

// CallProgress is the same as Call with the addition of a progress callback
// function as the last parameter.
//
// A caller can indicate its willingness to receive progressive results by
// calling CallProgress and supplying a callback function to handle progressive
// results that are returned before the final result.  Like Call(),
// CallProgress() returns the when the final result is returned by the callee.
// The progress callback is guaranteed not to be called after CallProgress()
// returns.
//
// There is no need to set the "receive_progress" option, as this is
// automatically set if a progress callback is provided.
//
// IMPORTANT: If the context has a timeout, then this needs to be sufficient to
// receive all progressive results as well as the final result.
func (c *Client) CallProgress(ctx context.Context, procedure string, options wamp.Dict, args wamp.List, kwargs wamp.Dict, cancelMode string, progcb ProgressCallback) (*wamp.Result, error) {
	switch cancelMode {
	case wamp.CancelModeKill, wamp.CancelModeKillNoWait, wamp.CancelModeSkip:
	case "":
		cancelMode = wamp.CancelModeKillNoWait
	default:
		return nil, fmt.Errorf("cancel mode not one of: '%s', '%s', '%s'",
			wamp.CancelModeKill, wamp.CancelModeKillNoWait, wamp.CancelModeSkip)
	}

	if options == nil {
		options = wamp.Dict{}
	}

	// If caller is willing to receive progressive results, create a channel to
	// receive these on.  Then, start a goroutine to receive progressive
	// results and call the callback for each.
	var progChan chan *wamp.Result
	var progDone chan struct{}
	if progcb != nil {
		progChan = make(chan *wamp.Result)
		options[wamp.OptReceiveProgress] = true

		progDone = make(chan struct{})
		go func() {
			for result := range progChan {
				progcb(result)
			}
			close(progDone)
		}()
	}

	id := c.idGen.Next()
	c.expectReply(id)
	c.sess.Send(&wamp.Call{
		Request:     id,
		Procedure:   wamp.URI(procedure),
		Options:     options,
		Arguments:   args,
		ArgumentsKw: kwargs,
	})

	// Wait to receive RESULT message.
	var msg wamp.Message
	var err error
	msg, err = c.waitForReplyWithCancel(ctx, id, cancelMode, procedure, progChan)

	// Finish handling any remaining progressive results before returning the
	// final result.
	if progDone != nil {
		<-progDone
	}

	if err != nil {
		return nil, err
	}

	switch msg := msg.(type) {
	case *wamp.Result:
		return msg, nil
	case *wamp.Error:
		return nil, RPCError{msg, procedure}
	default:
		return nil, unexpectedMsgError(msg, wamp.RESULT)
	}
}

// RPCError is a wrapper for a WAMP ERROR message that is received as a result
// of a CALL.  This allows the client application to type assert the error to a
// RPCError and inspect the the ERROR message contents, as may be necessary to
// process an error response from the callee.
type RPCError struct {
	Err       *wamp.Error
	Procedure string
}

// Error implements the error interface, returning an error string for the
// RPCError.
func (rpce RPCError) Error() string {
	return fmt.Sprintf("calling remote procedure '%s': %s",
		rpce.Procedure, wampErrorString(rpce.Err))
}

// Close causes the client to leave the realm it has joined, and closes the
// connection to the router.
func (c *Client) Close() error {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return errors.New("already closed")
	}

	// Cancel any running invocation handlers and wait for them to finish.
	// This makes it safe to close the actionChan.  Do this before leaving the
	// realm so that any invocation handlers do not hang waiting to send to the
	// router after it has stopped receiving from the client.
	close(c.stopping)
	c.activeInvHandlers.Wait()

	// Stop waiting for replies and clear the reply channel of pending writes.
	var awaitingReply map[wamp.ID]chan wamp.Message
	c.replyLock.Lock()
	awaitingReply = c.awaitingReply
	c.awaitingReply = nil
	for _, ch := range awaitingReply {
		select {
		case <-ch:
		default:
		}
	}
	c.replyLock.Unlock()

	// Leave the realm and stop the client's main goroutine.
	c.leaveRealm()

	// The run goroutine is guaranteed to have exited when leaveRealm()
	// returns, so there will be nothing trying to write to the reply channel.
	// Closing the channels dismisses any possible readers.
	if len(awaitingReply) != 0 {
		c.replyLock.Lock()
		for _, ch := range awaitingReply {
			close(ch)
		}
		c.replyLock.Unlock()
	}

	// c.sess.Peer is accessed from several go routines and would require
	// synchronisation if updated here.  Peer is closed in leaveRealm() so we
	// do not need to set it to nil.
	return nil
}

// RouterGoodbye returns the GOODBYE message received from the router, if one
// was received.  The client must be disconnected from the router first, so
// first check that the channel returned by client.Done() is closed before
// calling this function.
func (c *Client) RouterGoodbye() *wamp.Goodbye {
	select {
	case <-c.done:
	default:
		// Client not disconnected from router yet.
		return nil
	}
	return c.routerGoodbye
}

// SendProgress is used by a Callee client to return progressive RPC results.
//
// IMPORTANT: The context passed into SendProgress MUST be the same context
// that was passed into the invocation handler.  This context is responsible
// for associating progressive results with the call in progress.
func (c *Client) SendProgress(ctx context.Context, args wamp.List, kwArgs wamp.Dict) error {
	// Lookup the request ID using ctx.  If there is no request ID, this means
	// that the caller is not accepting progressive results, or that the
	// invocation handler has been closed because the call was canceled.
	var req wamp.ID
	var ok bool
	c.progGateLock.Lock()
	req, ok = c.progGate[ctx]
	c.progGateLock.Unlock()

	if !ok {
		// Caller is not accepting progressive results or call canceled.
		return errors.New("caller not accepting progressive results")
	}
	return c.sess.SendCtx(ctx, &wamp.Yield{
		Request:     req,
		Options:     wamp.Dict{wamp.OptProgress: true},
		Arguments:   args,
		ArgumentsKw: kwArgs,
	})
}

// joinRealm joins a WAMP realm, handling challenge/response authentication if
// needed.  The authHandlers portion of cfg supplies a map of WAMP authmethod
// names to functions that handle each auth type.  This can be nil if router is
// expected to allow anonymous authentication.
func joinRealm(peer wamp.Peer, cfg Config) (*wamp.Welcome, error) {
	if cfg.Realm == "" {
		return nil, errors.New("realm not specified")
	}
	details := cfg.HelloDetails
	if details == nil {
		details = wamp.Dict{}
	}
	if _, ok := details[helloRoles]; !ok {
		details[helloRoles] = clientRoles
	}
	if len(cfg.AuthHandlers) > 0 {
		authmethods := make(wamp.List, len(cfg.AuthHandlers))
		var i int
		for am := range cfg.AuthHandlers {
			authmethods[i] = am
			i++
		}
		details[helloAuthmethods] = authmethods
	}

	peer.Send(&wamp.Hello{Realm: wamp.URI(cfg.Realm), Details: details})
	msg, err := wamp.RecvTimeout(peer, cfg.ResponseTimeout)
	if err != nil {
		return nil, err
	}

	// Only expect CHALLENGE if client offered authmethod(s).
	if len(cfg.AuthHandlers) > 0 {
		// See if router sent CHALLENGE in response to client HELLO.
		if challenge, ok := msg.(*wamp.Challenge); ok {
			msg, err = handleCRAuth(peer, challenge, cfg.AuthHandlers,
				cfg.ResponseTimeout)
			if err != nil {
				return nil, err
			}
		}
		// Do not error if the message is not a CHALLENGE, as the auth methods
		// may have allowed the router to authenticate without CR auth.
	}

	welcome, ok := msg.(*wamp.Welcome)
	if !ok {
		// Received unexpected message from router.
		return nil, unexpectedMsgError(msg, wamp.WELCOME)
	}
	return welcome, nil
}

// leaveRealm leaves the current realm without closing the connection to the
// router.
func (c *Client) leaveRealm() {
	select {
	case <-c.done: // run already exited, client already disconnected
		return
	default:
	}

	// Send GOODBYE to router.  The router will respond with a GOODBYE message
	// which is handled by receiveFromRouter, and causes run() to exit.
	//
	// Make an effort to say goodbye, but do not wait around if blocked.
	c.sess.TrySend(&wamp.Goodbye{
		Details: wamp.Dict{},
		Reason:  wamp.CloseRealm,
	})

	// Close the peer.  This causes run() to exit if it has not already done so
	// after receiving GOODBYE from router.
	c.sess.Close()

	// Wait for run() to exit, but do not wait longer that a normal response
	// timeout.
	timer := time.NewTimer(c.responseTimeout)
	select {
	case <-c.done:
		timer.Stop()
	case <-timer.C:
		close(c.actionChan) // force run() to exit
		<-c.done
	}
}

func handleCRAuth(peer wamp.Peer, challenge *wamp.Challenge, authHandlers map[string]AuthFunc, rspTimeout time.Duration) (wamp.Message, error) {
	// Look up the authentication function for the specified authmethod.
	authFunc, ok := authHandlers[challenge.AuthMethod]
	if !ok {
		// The router send a challenge for an auth method the client does not
		// know how to deal with.  In response to a CHALLENGE message, the
		// Client MUST send an AUTHENTICATE message.  So, send empty
		// AUTHENTICATE since client does not know what to put in it.
		peer.Send(&wamp.Authenticate{})
	} else {
		// Create signature and send AUTHENTICATE.
		signature, authDetails := authFunc(challenge)
		peer.Send(&wamp.Authenticate{
			Signature: signature,
			Extra:     authDetails,
		})
	}
	msg, err := wamp.RecvTimeout(peer, rspTimeout)
	if err != nil {
		return nil, err
	}
	// If router sent back ABORT in response to client's authentication attempt
	// return error.
	if abort, ok := msg.(*wamp.Abort); ok {
		authErr, _ := wamp.AsString(abort.Details[wamp.OptError])
		if authErr == "" {
			authErr = "authentication failed"
		}
		return nil, errors.New(authErr)
	}

	// Return the router's response to AUTHENTICATE, this should be WELCOME.
	return msg, nil
}

// wampErrorString creates a message string that combines the Error, Arguments,
// and the ArgumentsKw fields from a wamp.Error message.
func wampErrorString(werr *wamp.Error) string {
	e := fmt.Sprintf("%v", werr.Error)
	if len(werr.Arguments) != 0 {
		// Append ": arg1, arg2, ..., argN"
		args := make([]string, len(werr.Arguments))
		for i := range werr.Arguments {
			s, ok := wamp.AsString(werr.Arguments[i])
			if !ok {
				s = fmt.Sprint(werr.Arguments[i])
			}
			args[i] = s
		}
		e += fmt.Sprintf(": %s", strings.Join(args, ", "))
	}
	if len(werr.ArgumentsKw) != 0 {
		// Append ": k1=v1, k2=v2, ..., kN=vN"
		kws := make([]string, len(werr.ArgumentsKw))
		var i int
		for k, v := range werr.ArgumentsKw {
			ks, ok := wamp.AsString(k)
			if !ok {
				ks = fmt.Sprint(k)
			}
			vs, ok := wamp.AsString(v)
			if !ok {
				vs = fmt.Sprint(v)
			}
			kws[i] = fmt.Sprint(ks, "=", vs)
			i++
		}
		e += fmt.Sprintf(": %s", strings.Join(kws, ", "))
	}
	return e
}

// unexpectedMsgError creates an error with information about the unexpected
// message that was received from the router.
func unexpectedMsgError(msg wamp.Message, expected wamp.MessageType) error {
	s := fmt.Sprint("received unexpected ", msg.MessageType(),
		" message when expecting ", expected)

	var details wamp.Dict
	var reason string
	switch m := msg.(type) {
	case *wamp.Abort:
		reason = string(m.Reason)
		details = m.Details
	case *wamp.Goodbye:
		reason = string(m.Reason)
		details = m.Details
	}
	var extra []string
	if reason != "" {
		extra = append(extra, reason)
	}
	if len(details) != 0 {
		var ds []string
		for k, v := range details {
			ds = append(ds, fmt.Sprintf("%s=%v", k, v))
		}
		extra = append(extra, strings.Join(ds, " "))
	}
	if len(extra) != 0 {
		s = fmt.Sprint(s, ": ", strings.Join(extra, " "))
	}
	return errors.New(s)
}

func (c *Client) expectReply(id wamp.ID) {
	wait := make(chan wamp.Message)
	c.replyLock.Lock()
	if c.awaitingReply != nil { // Client has already been closed
		c.awaitingReply[id] = wait
	}
	c.replyLock.Unlock()
}

// waitForReply waits for an expected reply from the router.
//
// IMPORTANT: Must not block on anything requiring run() goroutine, since the
// run() goroutine may be blocked waiting for a reply to be read from the
// awaiting reply channel.
func (c *Client) waitForReply(id wamp.ID) (wamp.Message, error) {
	var wait chan wamp.Message
	var ok bool
	c.replyLock.Lock()
	wait, ok = c.awaitingReply[id]
	c.replyLock.Unlock()
	if !ok {
		return nil, fmt.Errorf("not expecting reply for ID: %v", id)
	}

	var msg wamp.Message
	var err error
	timer := time.NewTimer(c.responseTimeout)
	select {
	case msg, ok = <-wait:
		timer.Stop()
		if !ok {
			// Return directly here, since awaitingReply entry already deleted.
			return nil, errors.New("client closed")
		}
	case <-timer.C:
		err = errors.New("timeout waiting for reply")
	}
	c.replyLock.Lock()
	delete(c.awaitingReply, id)
	c.replyLock.Unlock()

	return msg, err
}

// waitForReplyWithCancel waits for an expected reply from the router while
// monitoring the context for call cancellation.
//
// IMPORTANT: Must not block on anything requiring run() goroutine, since the
// run() goroutine may be blocked waiting for a reply to be read from the
// awaiting reply channel.
func (c *Client) waitForReplyWithCancel(ctx context.Context, id wamp.ID, mode, procedure string, progChan chan *wamp.Result) (wamp.Message, error) {
	if progChan != nil {
		defer close(progChan)
	}
	var wait chan wamp.Message
	var ok bool
	c.replyLock.Lock()
	wait, ok = c.awaitingReply[id]
	c.replyLock.Unlock()
	if !ok {
		return nil, fmt.Errorf("not expecting reply for ID: %v", id)
	}

	var msg wamp.Message
	var err error
CollectResults:
	select {
	case msg, ok = <-wait:
		if !ok {
			// Return here, since awaitingReply entry already deleted.
			return nil, errors.New("client closed")
		}
		// If this is a progressive result, put the Result message on the
		// progress channel and go back to waiting for more results.
		if progChan != nil {
			var result *wamp.Result
			if result, ok = msg.(*wamp.Result); ok {
				if ok, _ = result.Details[wamp.OptProgress].(bool); ok {
					progChan <- result
					goto CollectResults
				}
			}
		}
	case <-ctx.Done():
		err = ctx.Err()
		if c.debug {
			c.log.Printf("Call to %q canceled by caller (mode=%s): %s",
				procedure, mode, err)
		}
		c.sess.Send(&wamp.Cancel{
			Request: id,
			Options: wamp.SetOption(nil, wamp.OptMode, mode),
		})
		// Wait for the ERROR from the dealer.
		timer := time.NewTimer(c.responseTimeout)
	waitCancel:
		// Discard responses until ERROR or timeout
		select {
		case msg, ok = <-wait:
			if !ok {
				timer.Stop()
				return nil, err
			}
			if _, ok = msg.(*wamp.Error); !ok {
				// Discard message
				goto waitCancel
			}
			timer.Stop()
		case <-timer.C:
			// Did not get expected response to cancel
			err = errors.New("timeout waiting for reply after cancel")
		}
	}
	// All done with this call, so not waiting for more replies.
	c.replyLock.Lock()
	delete(c.awaitingReply, id)
	c.replyLock.Unlock()

	return msg, err
}

// run is the core client goroutine.  This handles messages received from the
// router and serializes access to all mutable state.
func (c *Client) run() {
	defer close(c.done)
	if c.debug {
		defer c.log.Println("Client", c.sess, "closed")
	}
	recvChan := c.sess.Recv()
	for {
		select {
		case action, ok := <-c.actionChan:
			if !ok {
				return
			}
			action()
		case msg, ok := <-recvChan:
			if !ok {
				return
			}
			if c.runReceiveFromRouter(msg) {
				return
			}
		}
	}
}

// ----------------------------------------------------------------------------
// All functions below access internal mutable state and must be executed by
// the run() goroutine.
//

// runReceiveFromRouter handles messages from the router.  Returns true if
// client needs to close due to receiving GOODBYE from router.
func (c *Client) runReceiveFromRouter(msg wamp.Message) bool {
	if c.debug {
		c.log.Println("Client", c.sess, "received", msg.MessageType())
	}
	switch msg := msg.(type) {
	case *wamp.Event:
		c.runHandleEvent(msg)

	case *wamp.Invocation:
		c.runHandleInvocation(msg)
	case *wamp.Interrupt:
		c.runHandleInterrupt(msg)

	case *wamp.Registered:
		c.runSignalReply(msg, msg.Request)
	case *wamp.Subscribed:
		c.runSignalReply(msg, msg.Request)
	case *wamp.Unsubscribed:
		c.runSignalReply(msg, msg.Request)
	case *wamp.Unregistered:
		c.runSignalReply(msg, msg.Request)
	case *wamp.Result:
		c.runSignalReply(msg, msg.Request)
	case *wamp.Published:
		c.runSignalReply(msg, msg.Request)
	case *wamp.Error:
		c.runSignalReply(msg, msg.Request)

	case *wamp.Goodbye:
		c.routerGoodbye = msg
		return true

	default:
		c.log.Println("Unhandled message from router:", msg.MessageType(), msg)
	}
	return false
}

// runHandleEvent calls the event handler function that a subscriber designated
// for handling EVENT messages.
//
// The eventHandlers are called serially so that they execute in the same order
// as the messages are received in.  This could not be guaranteed if executing
// concurrently in separate goroutines.
func (c *Client) runHandleEvent(msg *wamp.Event) {
	handler, ok := c.eventHandlers[msg.Subscription]
	if !ok {
		c.log.Println("No handler registered for subscription:",
			msg.Subscription)
		return
	}
	handler(msg.Arguments, msg.ArgumentsKw, msg.Details)
}

// runHandleInvocation processes an INVOCATION message from the router
// requesting a call to a registered RPC procedure.
func (c *Client) runHandleInvocation(msg *wamp.Invocation) {
	handler, ok := c.invHandlers[msg.Registration]
	if !ok {
		errMsg := fmt.Sprintf("Client has no handler for registration %v",
			msg.Registration)
		// The dealer has a procedure registered to this client, but this
		// client does not recognize the registration ID.  This is not
		// reported as ErrNoSuchProcedure, since the dealer has a procedure
		// registered.  It is reported as ErrInvalidArgument to denote that
		// the client has a problem with the registration ID argument.
		c.sess.Send(&wamp.Error{
			Type:      wamp.INVOCATION,
			Request:   msg.Request,
			Details:   wamp.Dict{},
			Error:     wamp.ErrInvalidArgument,
			Arguments: wamp.List{errMsg},
		})
		c.log.Print(errMsg)
		return
	}

	// Create a kill switch so that invocation can be canceled.
	var cancel context.CancelFunc
	var ctx context.Context
	timeout, _ := wamp.AsInt64(msg.Details[wamp.OptTimeout])
	if timeout > 0 {
		// The caller specified a timeout, in milliseconds.
		ctx, cancel = context.WithTimeout(context.Background(),
			time.Millisecond*time.Duration(timeout))
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	c.invHandlerKill[msg.Request] = cancel
	c.activeInvHandlers.Add(1)

	// If caller is accepting progressive results, create map entry to
	// allow progress to be sent.
	if ok, _ = msg.Details[wamp.OptReceiveProgress].(bool); ok {
		c.progGateLock.Lock()
		c.progGate[ctx] = msg.Request
		c.progGateLock.Unlock()
	}

	// Start a goroutine to run the user-defined invocation handler.
	go func() {
		defer cancel()

		// Create channel to hold result.  Channel must be buffered.
		// Otherwise, canceling the call will leak the goroutine that is
		// blocked forever waiting to send the result to the channel.
		resChan := make(chan *InvokeResult, 1)
		go func() {
			// The Context is passed into the handler to tell the client
			// application to stop whatever it is doing if it cares to pay
			// attention.
			resChan <- handler(ctx, msg.Arguments, msg.ArgumentsKw,
				msg.Details)
		}()

		// Remove the kill switch when done processing invocation.
		defer func() {
			c.progGateLock.Lock()
			delete(c.progGate, ctx)
			c.progGateLock.Unlock()

			c.runAction(func() {
				delete(c.invHandlerKill, msg.Request)
				c.activeInvHandlers.Done()
			})
		}()

		// Wait for the handler to finish or for the call be to canceled.
		var result *InvokeResult
		select {
		case result = <-resChan:
			// If the handler returns a nil result, this means the handler
			// canceled the call.
			if result == nil {
				result = &InvokeResult{Err: wamp.ErrCanceled}
				c.log.Println("INVOCATION", msg.Request, "canceled by callee")
			}
		case <-c.stopping:
			c.log.Print("Client stopping, invocation handler canceled")
			// Return without sending response to server.  This will also
			// cancel the context.
			return
		case <-ctx.Done():
			// Received an INTERRUPT message from the router.
			// Note: handler is also just as likely to return on INTERRUPT.
			result = &InvokeResult{Err: wamp.ErrCanceled}
			var reason string
			if ctx.Err() == context.DeadlineExceeded {
				reason = "callee due to timeout"
			} else {
				reason = "router"
			}
			c.log.Println("INVOCATION", msg.Request, "canceled by", reason)
		}

		if result.Err != "" {
			c.sess.Send(&wamp.Error{
				Type:        wamp.INVOCATION,
				Request:     msg.Request,
				Details:     wamp.Dict{},
				Arguments:   result.Args,
				ArgumentsKw: result.Kwargs,
				Error:       result.Err,
			})
			return
		}
		c.sess.Send(&wamp.Yield{
			Request:     msg.Request,
			Options:     wamp.Dict{},
			Arguments:   result.Args,
			ArgumentsKw: result.Kwargs,
		})
	}()
}

// runHandleInterrupt processes an INTERRUPT message from the router,
// requesting that a pending call be canceled.
func (c *Client) runHandleInterrupt(msg *wamp.Interrupt) {
	logMsg := "Received INTERRUPT for INVOCATION"
	cancel, ok := c.invHandlerKill[msg.Request]
	if !ok {
		c.log.Println(logMsg, msg.Request, "that no longer exists")
		return
	}
	if reason, ok := wamp.AsURI(msg.Options[wamp.OptReason]); ok {
		c.log.Println(logMsg, msg.Request, "reason:", reason)
	} else {
		c.log.Println(logMsg, msg.Request)
	}
	cancel()
}

func (c *Client) runSignalReply(msg wamp.Message, requestID wamp.ID) {
	var w chan wamp.Message
	var ok bool
	c.replyLock.Lock()
	w, ok = c.awaitingReply[requestID]
	c.replyLock.Unlock()
	if !ok {
		c.log.Println("Received", msg.MessageType(), requestID,
			"that client is no longer waiting for")
		return
	}
	w <- msg
}
