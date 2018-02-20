package device

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Comcast/webpa-common/convey/conveyhttp"
	"github.com/Comcast/webpa-common/logging"
	"github.com/Comcast/webpa-common/wrp"
	"github.com/Comcast/webpa-common/xhttp"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gorilla/websocket"
)

const MaxDevicesHeader = "X-Xmidt-Max-Devices"

var authStatus *websocket.PreparedMessage

func init() {
	var err error
	authStatus, err = websocket.NewPreparedMessage(
		websocket.BinaryMessage,
		wrp.MustEncode(&wrp.AuthorizationStatus{Status: wrp.AuthStatusAuthorized}, wrp.Msgpack),
	)

	if err != nil {
		panic(err)
	}
}

// Connector is a strategy interface for managing device connections to a server.
// Implementations are responsible for upgrading websocket connections and providing
// for explicit disconnection.
type Connector interface {
	// Connect upgrade an HTTP connection to a websocket and begins concurrent
	// management of the device.
	Connect(http.ResponseWriter, *http.Request, http.Header) (Interface, error)

	// Disconnect disconnects the device associated with the given id.
	// If the id was found, this method returns true.
	Disconnect(ID) bool

	// DisconnectIf iterates over all devices known to this manager, applying the
	// given predicate.  For any devices that result in true, this method disconnects them.
	// Note that this method may pause connections and disconnections while it is executing.
	// This method returns the number of devices that were disconnected.
	//
	// Only disconnection by ID is supported, which means that any identifier matching
	// the predicate will result in *all* duplicate devices under that ID being removed.
	//
	// No methods on this Manager should be called from within the predicate function, or
	// a deadlock will likely occur.
	DisconnectIf(func(ID) bool) int
}

// Router handles dispatching messages to devices.
type Router interface {
	// Route dispatches a WRP request to exactly one device, identified by the ID
	// field of the request.  Route is synchronous, and honors the cancellation semantics
	// of the Request's context.
	Route(*Request) (*Response, error)
}

// Registry is the strategy interface for querying the set of connected devices.  Methods
// in this interface follow the Visitor pattern and are typically executed under a read lock.
type Registry interface {
	// Get returns the device associated with the given ID, if any
	Get(ID) (Interface, bool)

	// VisitAll applies the given visitor function to each device known to this manager.
	//
	// No methods on this Manager should be called from within the visitor function, or
	// a deadlock will likely occur.
	VisitAll(func(Interface)) int
}

// Manager supplies a hub for connecting and disconnecting devices as well as
// an access point for obtaining device metadata.
type Manager interface {
	Connector
	Router
	Registry
}

// NewManager constructs a Manager from a set of options.  A ConnectionFactory will be
// created from the options if one is not supplied.
func NewManager(o *Options) Manager {
	var (
		logger   = o.logger()
		measures = NewMeasures(o.metricsProvider())
	)

	return &manager{
		logger:   logger,
		errorLog: logging.Error(logger),
		debugLog: logging.Debug(logger),

		readDeadline:     NewDeadline(o.idlePeriod(), o.now()),
		writeDeadline:    NewDeadline(o.writeTimeout(), o.now()),
		upgrader:         o.upgrader(),
		conveyTranslator: conveyhttp.NewHeaderTranslator("", nil),
		devices: newRegistry(registryOptions{
			Logger:   logger,
			Limit:    o.maxDevices(),
			Measures: measures,
		}),
		deviceMessageQueueSize: o.deviceMessageQueueSize(),
		pingPeriod:             o.pingPeriod(),
		authDelay:              o.authDelay(),

		listeners: o.listeners(),
		measures:  measures,
	}
}

// manager is the internal Manager implementation.
type manager struct {
	logger   log.Logger
	errorLog log.Logger
	debugLog log.Logger

	readDeadline     func() time.Time
	writeDeadline    func() time.Time
	upgrader         *websocket.Upgrader
	conveyTranslator conveyhttp.HeaderTranslator

	devices *registry

	deviceMessageQueueSize int
	pingPeriod             time.Duration
	authDelay              time.Duration

	listeners []Listener
	measures  Measures
}

func (m *manager) Connect(response http.ResponseWriter, request *http.Request, responseHeader http.Header) (Interface, error) {
	m.debugLog.Log(logging.MessageKey(), "device connect", "url", request.URL)
	id, ok := GetID(request.Context())
	if !ok {
		xhttp.WriteError(
			response,
			http.StatusInternalServerError,
			ErrorMissingDeviceNameContext,
		)

		return nil, ErrorMissingDeviceNameContext
	}

	d := newDevice(deviceOptions{ID: id, QueueSize: m.deviceMessageQueueSize, Logger: m.logger})
	if convey, err := m.conveyTranslator.FromHeader(request.Header); err == nil {
		d.debugLog.Log(level.Key(), level.DebugValue(), "id", id, "convey", convey)
	} else if err != conveyhttp.ErrMissingHeader {
		d.errorLog.Log(level.Key(), level.ErrorValue(), logging.MessageKey(), "badly formatted convey data", "id", id, logging.ErrorKey(), err)
	}

	c, err := m.upgrader.Upgrade(response, request, responseHeader)
	if err != nil {
		d.errorLog.Log(logging.MessageKey(), "failed websocket upgrade", logging.ErrorKey(), err)
		return nil, err
	}

	d.debugLog.Log(logging.MessageKey(), "websocket upgrade complete", "localAddress", c.LocalAddr().String())

	pinger, err := NewPinger(c, m.measures.Ping, []byte(d.ID()), m.writeDeadline)
	if err != nil {
		d.errorLog.Log(logging.MessageKey(), "unable to create pinger", logging.ErrorKey(), err)
		c.Close()
		return nil, err
	}

	if err := m.devices.add(d); err != nil {
		d.errorLog.Log(logging.MessageKey(), "unable to register device", logging.ErrorKey(), err)
		c.Close()
		return nil, err
	}

	m.dispatch(
		&Event{
			Type:   Connect,
			Device: d,
		},
	)

	SetPongHandler(c, m.measures.Pong, m.readDeadline)
	closeOnce := new(sync.Once)
	go m.readPump(d, InstrumentReader(c, d.statistics), closeOnce)
	go m.writePump(d, InstrumentWriter(c, d.statistics), pinger, closeOnce)

	return d, nil
}

func (m *manager) dispatch(e *Event) {
	for _, listener := range m.listeners {
		listener(e)
	}
}

// pumpClose handles the proper shutdown and logging of a device's pumps.
// This method should be executed within a sync.Once, so that it only executes
// once for a given device.
//
// Note that the write pump does additional cleanup.  In particular, the write pump
// dispatches message failed events for any messages that were waiting to be delivered
// at the time of pump closure.
func (m *manager) pumpClose(d *device, c io.Closer, pumpError error) {
	if pumpError != nil {
		d.errorLog.Log(logging.MessageKey(), "pump close due to error", logging.ErrorKey(), pumpError)
	} else {
		d.debugLog.Log(logging.MessageKey(), "pump close")
	}

	// remove will invoke requestClose()
	m.devices.remove(d.id)

	if closeError := c.Close(); closeError != nil {
		d.errorLog.Log(logging.MessageKey(), "Error closing device connection", logging.ErrorKey(), closeError)
	} else {
		d.debugLog.Log(logging.MessageKey(), "Closed device connection")
	}

	m.dispatch(
		&Event{
			Type:   Disconnect,
			Device: d,
		},
	)
}

// readPump is the goroutine which handles the stream of WRP messages from a device.
// This goroutine exits when any error occurs on the connection.
func (m *manager) readPump(d *device, r ReadCloser, closeOnce *sync.Once) {
	defer d.debugLog.Log(logging.MessageKey(), "readPump exiting")
	d.debugLog.Log(logging.MessageKey(), "readPump starting")

	var (
		readError error
		decoder   = wrp.NewDecoder(nil, wrp.Msgpack)
	)

	// all the read pump has to do is ensure the device and the connection are closed
	// it is the write pump's responsibility to do further cleanup
	defer closeOnce.Do(func() { m.pumpClose(d, r, readError) })

	for {
		messageType, data, readError := r.ReadMessage()
		if readError != nil {
			d.errorLog.Log(logging.MessageKey(), "read error", logging.ErrorKey(), readError)
			return
		}

		if messageType != websocket.BinaryMessage {
			d.errorLog.Log(logging.MessageKey(), "skipping non-binary frame", "messageType", messageType)
			continue
		}

		var (
			message = new(wrp.Message)
			event   = Event{
				Type:     MessageReceived,
				Device:   d,
				Message:  message,
				Format:   wrp.Msgpack,
				Contents: data,
			}
		)

		decoder.ResetBytes(data)
		err := decoder.Decode(message)
		decoder.ResetBytes(nil)
		if err != nil {
			d.errorLog.Log(logging.MessageKey(), "skipping malformed WRP message", logging.ErrorKey(), err)
			continue
		}

		if message.Type == wrp.SimpleRequestResponseMessageType {
			m.measures.RequestResponse.Add(1.0)
		}

		// update any waiting transaction
		if message.IsTransactionPart() {
			err := d.transactions.Complete(
				message.TransactionKey(),
				&Response{
					Device:   d,
					Message:  message,
					Format:   wrp.Msgpack,
					Contents: data,
				},
			)

			if err != nil {
				d.errorLog.Log(logging.MessageKey(), "Error while completing transaction", logging.ErrorKey(), err)
				event.Type = TransactionBroken
				event.Error = err
			} else {
				event.Type = TransactionComplete
			}
		}

		m.dispatch(&event)
	}
}

// writePump is the goroutine which services messages addressed to the device.
// this goroutine exits when either an explicit shutdown is requested or any
// error occurs on the connection.
func (m *manager) writePump(d *device, w WriteCloser, pinger func() error, closeOnce *sync.Once) {
	defer d.debugLog.Log(logging.MessageKey(), "writePump exiting")
	d.debugLog.Log(logging.MessageKey(), "writePump starting")

	var (
		envelope   *envelope
		encoder    = wrp.NewEncoder(nil, wrp.Msgpack)
		writeError error

		pingTicker = time.NewTicker(m.pingPeriod)

		// wait for the delay, then send an auth status request to the device
		authStatusTimer = time.AfterFunc(m.authDelay, func() {
			// TODO: This will keep the device from being garbage collected until the timer
			// triggers.  This is only a problem if a device connects then disconnects faster
			// than the authDelay setting.
			w.WritePreparedMessage(authStatus)
		})
	)

	// cleanup: we not only ensure that the device and connection are closed but also
	// ensure that any messages that were waiting and/or failed are dispatched to
	// the configured listener
	defer func() {
		pingTicker.Stop()
		authStatusTimer.Stop()
		closeOnce.Do(func() { m.pumpClose(d, w, writeError) })

		// notify listener of any message that just now failed
		// any writeError is passed via this event
		if envelope != nil {
			m.dispatch(&Event{
				Type:     MessageFailed,
				Device:   d,
				Message:  envelope.request.Message,
				Format:   envelope.request.Format,
				Contents: envelope.request.Contents,
				Error:    writeError,
			})
		}

		// drain the messages, dispatching them as message failed events.  we never close
		// the message channel, so just drain until a receive would block.
		//
		// Nil is passed explicitly as the error to indicate that these messages failed due
		// to the device disconnecting, not due to an actual I/O error.
		for {
			select {
			case undeliverable := <-d.messages:
				d.errorLog.Log(logging.MessageKey(), "undeliverable message", "deviceMessage", undeliverable)
				m.dispatch(&Event{
					Type:     MessageFailed,
					Device:   d,
					Message:  undeliverable.request.Message,
					Format:   undeliverable.request.Format,
					Contents: undeliverable.request.Contents,
					Error:    writeError,
				})
			default:
				return
			}
		}
	}()

	for writeError == nil {
		envelope = nil

		select {
		case <-d.shutdown:
			d.debugLog.Log(logging.MessageKey(), "explicit shutdown")
			writeError = w.Close()
			return

		case envelope = <-d.messages:
			var frameContents []byte
			if envelope.request.Format == wrp.Msgpack && len(envelope.request.Contents) > 0 {
				frameContents = envelope.request.Contents
			} else {
				// if the request was in a format other than Msgpack, or if the caller did not pass
				// Contents, then do the encoding here.
				encoder.ResetBytes(&frameContents)
				writeError = encoder.Encode(envelope.request.Message)
				encoder.ResetBytes(nil)
			}

			if writeError == nil {
				writeError = w.WriteMessage(websocket.BinaryMessage, frameContents)
			}

			event := Event{
				Device:   d,
				Message:  envelope.request.Message,
				Format:   envelope.request.Format,
				Contents: envelope.request.Contents,
				Error:    writeError,
			}

			if writeError != nil {
				envelope.complete <- writeError
				event.Type = MessageFailed
			} else {
				event.Type = MessageSent
			}

			close(envelope.complete)
			m.dispatch(&event)

		case <-pingTicker.C:
			writeError = pinger()
		}
	}
}

func (m *manager) Disconnect(id ID) bool {
	_, ok := m.devices.remove(id)
	return ok
}

func (m *manager) DisconnectIf(filter func(ID) bool) int {
	return m.devices.removeIf(func(d *device) bool {
		return filter(d.id)
	})
}

func (m *manager) Get(id ID) (Interface, bool) {
	return m.devices.get(id)
}

func (m *manager) VisitAll(visitor func(Interface)) int {
	return m.devices.visit(func(d *device) {
		visitor(d)
	})
}

func (m *manager) Route(request *Request) (*Response, error) {
	if destination, err := request.ID(); err != nil {
		return nil, err
	} else if d, ok := m.devices.get(destination); ok {
		return d.Send(request)
	} else {
		return nil, ErrorDeviceNotFound
	}
}
