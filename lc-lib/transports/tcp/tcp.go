/*
 * Copyright 2014-2015 Jason Woods.
 *
 * This file is a modification of code from Logstash Forwarder.
 * Copyright 2012-2013 Jordan Sissel and contributors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package transports

import (
	"bytes"
	"compress/zlib"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/driskell/log-courier/lc-lib/core"
	"github.com/driskell/log-courier/lc-lib/transports"
)

import _ "crypto/sha256" // Support for newer SSL signature algorithms
import _ "crypto/sha512" // Support for newer SSL signature algorithms

const (
	// Essentially, this is how often we should check for disconnect/shutdown during socket reads
	socketIntervalSeconds = 1
)

// TransportTCP implements a transport that sends over TCP
// It also can optionally introduce a TLS layer for security
type TransportTCP struct {
	config       *TransportTCPFactory
	finishOnFail bool
	socket       net.Conn
	tlsSocket    *tls.Conn
	tlsConfig    tls.Config
	backoff      *core.ExpBackoff

	controllerChan chan int
	observer       transports.Observer
	failChan       chan error

	wait        sync.WaitGroup
	sendControl chan int
	recvControl chan int

	sendChan chan []byte

	// Use in receiver routine only
	pongPending bool
	pongTimer   *time.Timer
}

// ReloadConfig returns true if the transport needs to be restarted in order
// for the new configuration to apply
func (t *TransportTCP) ReloadConfig(factoryInterface interface{}, finishOnFail bool) bool {
	newConfig := factoryInterface.(*TransportTCPFactory)
	t.finishOnFail = finishOnFail

	// TODO: Check timestamps of underlying certificate files to detect changes
	if newConfig.SSLCertificate != t.config.SSLCertificate || newConfig.SSLKey != t.config.SSLKey || newConfig.SSLCA != t.config.SSLCA {
		return true
	}

	// Only copy net config just in case something in the factory did change that
	// we didn't account for which does require a restart
	t.config.netConfig = newConfig.netConfig

	return false
}

// controller is the master routine which handles connection and reconnection
// When reconnecting, the socket and all routines are torn down and restarted.
// It also
func (t *TransportTCP) controller() {
	defer func() {
		t.sendEvent(nil, transports.NewStatusEvent(t.observer, transports.Finished))
	}()

	// Main connect loop
	for {
		var err error
		var shutdown bool

		shutdown, err = t.connect()
		if shutdown {
			t.disconnect()
			return
		}
		if err == nil {
			// Connected - sit and wait for shutdown or error
			t.backoff.Reset()

			select {
			// TODO: Handle configuration reload
			case <-t.controllerChan:
				// Shutdown request
				t.disconnect()
				return
			case err = <-t.failChan:
				// If err is nil, it's a forced failure by publisher so we need not
				// call observer fail to let it know about it
				if err == nil {
					err = transports.ErrForcedFailure
				}
			}
		}

		if err != nil {
			if t.finishOnFail {
				log.Errorf("[%s] Transport error: %s", t.observer.Pool().Server(), err)
				t.disconnect()
				return
			}

			log.Errorf("[%s] Transport error, reconnecting: %s", t.observer.Pool().Server(), err)
		} else {
			log.Info("[%s] Transport reconnecting", t.observer.Pool().Server())
		}

		t.disconnect()

		if t.sendEvent(t.controllerChan, transports.NewStatusEvent(t.observer, transports.Failed)) {
			return
		}

		// If this returns false, we are shutting down
		if t.reconnectWait() {
			continue
		}

		return
	}
}

// reconnectWait waits the reconnect timeout before attempting to reconnect.
// It also monitors for shutdown and configuration reload events while waiting.
func (t *TransportTCP) reconnectWait() bool {
	now := time.Now()
	reconnectDue := now.Add(t.backoff.Trigger())

ReconnectWaitLoop:
	select {
	// TODO: Handle configuration reload
	case <-t.controllerChan:
		// Shutdown request
		return false
	case <-time.After(reconnectDue.Sub(now)):
		break ReconnectWaitLoop
	}

	return true
}

// connect connects the socket and starts the sender and receiver routines
// Returns an error and also true if shutdown was detected
func (t *TransportTCP) connect() (bool, error) {
	if t.sendControl != nil {
		t.disconnect()
	}

	addr, err := t.observer.Pool().Next()
	if err != nil {
		return false, fmt.Errorf("Failed to select next address: %s", err)
	}

	desc := t.observer.Pool().Desc()

	log.Info("[%s] Attempting to connect to %s", t.observer.Pool().Server(), desc)

	tcpsocket, err := net.DialTimeout("tcp", addr.String(), t.config.netConfig.Timeout)
	if err != nil {
		return false, fmt.Errorf("Failed to connect to %s: %s", desc, err)
	}

	// Now wrap in TLS if this is the TLS transport
	if t.config.transport == TransportTCPTLS {
		// Disable SSLv3 (mitigate POODLE vulnerability)
		t.tlsConfig.MinVersion = tls.VersionTLS10

		// Set the certificate if we set one
		if t.config.certificate != nil {
			t.tlsConfig.Certificates = []tls.Certificate{*t.config.certificate}
		} else {
			t.tlsConfig.Certificates = nil
		}

		// Set CA for server verification
		t.tlsConfig.RootCAs = x509.NewCertPool()
		for _, cert := range t.config.caList {
			t.tlsConfig.RootCAs.AddCert(cert)
		}

		// Set the tlsConfig server name for server validation (required since Go 1.3)
		t.tlsConfig.ServerName = t.observer.Pool().Host()

		t.tlsSocket = tls.Client(&transportTCPWrap{transport: t, tcpsocket: tcpsocket}, &t.tlsConfig)
		t.tlsSocket.SetDeadline(time.Now().Add(t.config.netConfig.Timeout))
		err = t.tlsSocket.Handshake()
		if err != nil {
			t.tlsSocket.Close()
			tcpsocket.Close()
			t.checkClientCertificates()
			return false, fmt.Errorf("TLS Handshake failure with %s: %s", desc, err)
		}

		t.socket = t.tlsSocket
	} else {
		t.socket = tcpsocket
	}

	log.Notice("[%s] Connected to %s", t.observer.Pool().Server(), desc)

	// Signal channels
	t.sendControl = make(chan int, 1)
	t.recvControl = make(chan int, 1)
	t.sendChan = make(chan []byte, t.config.netConfig.MaxPendingPayloads)

	// Failure channel - ensure we can fit 2 errors here, one from sender and one
	// from receive - otherwise if both fail at the same time, disconnect blocks
	// NOTE: This may not be necessary anymore - both recv_chan pushes also select
	//       on the shutdown channel, which will close on the first error returned
	t.failChan = make(chan error, 2)

	t.wait.Add(2)

	// Start separate sender and receiver so we can asynchronously send and
	// receive for max performance. They have to be different routines too because
	// we don't have cross-platform poll, so they will need to block. Of course,
	// we'll time out and check shutdown on occasion
	go t.sender()
	go t.receiver()

	return false, nil
}

// checkClientCertificates logs a warning if it finds any certificates that are
// not currently valid
func (t *TransportTCP) checkClientCertificates() {
	if t.config.certificateList == nil {
		// No certificates were specified, don't do anything
		return
	}

	now := time.Now()
	certIssues := false

	for _, cert := range t.config.certificateList {
		if cert.NotBefore.After(now) {
			log.Warning("The client certificate with common name '%s' is not valid until %s.", cert.Subject.CommonName, cert.NotBefore.Format("Jan 02 2006"))
			certIssues = true
		}

		if cert.NotAfter.Before(now) {
			log.Warning("The client certificate with common name '%s' expired on %s.", cert.Subject.CommonName, cert.NotAfter.Format("Jan 02 2006"))
			certIssues = true
		}
	}

	if certIssues {
		log.Warning("Client certificate issues may be preventing successful connection.")
	}
}

// disconnect shuts down the sender and receiver routines and disconnects the
// socket
func (t *TransportTCP) disconnect() {
	if t.sendControl == nil {
		return
	}

	// Send shutdown request
	close(t.sendControl)
	close(t.recvControl)
	t.wait.Wait()
	t.sendControl = nil
	t.recvControl = nil

	// If tls, shutdown tls socket first
	if t.config.transport == TransportTCPTLS {
		t.tlsSocket.Close()
	}

	t.socket.Close()

	log.Notice("[%s] Disconnected from %s", t.observer.Pool().Server(), t.observer.Pool().Desc())
}

// sender handles socket writes
func (t *TransportTCP) sender() {
	defer func() {
		t.wait.Done()
	}()

	// Send a started signal to say we're ready to receive events
	if t.sendEvent(t.controllerChan, transports.NewStatusEvent(t.observer, transports.Started)) {
		return
	}

SenderLoop:
	for {
		select {
		case <-t.sendControl:
			// Shutdown
			break SenderLoop
		case msg := <-t.sendChan:
			// Write deadline is managed by our net.Conn wrapper that TLS will call
			// into and keeps retrying writes until timeout or error
			_, err := t.socket.Write(msg)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Shutdown will have been received by the wrapper
					break SenderLoop
				}
				// Fail the transport
				select {
				case <-t.sendControl:
				case t.failChan <- err:
				}
				break SenderLoop
			}
		}
	}
}

// receiver handles socket reads
func (t *TransportTCP) receiver() {
	defer func() {
		t.wait.Done()
	}()

	var err error
	var shutdown bool
	var message []byte

	header := make([]byte, 8)

ReceiverLoop:
	for {
		if shutdown, err = t.receiverRead(header); shutdown || err != nil {
			break
		}

		// Grab length of message
		length := binary.BigEndian.Uint32(header[4:8])

		// Sanity
		if length > 1048576 {
			err = fmt.Errorf("Data too large (%d)", length)
			break
		}

		if length > 0 {
			// Allocate for full message
			message = make([]byte, length)
			if shutdown, err = t.receiverRead(message); shutdown || err != nil {
				break
			}
		} else {
			message = []byte("")
		}

		switch {
		case bytes.Compare(header[0:4], []byte("PONG")) == 0:
			if t.sendEvent(t.recvControl, transports.NewPongEvent(t.observer)) {
				break ReceiverLoop
			}
		case bytes.Compare(header[0:4], []byte("ACKN")) == 0:
			if len(message) != 20 {
				err = fmt.Errorf("Protocol error: Corrupt message (ACKN size %d != 20)", len(message))
				break ReceiverLoop
			}

			if t.sendEvent(t.recvControl, transports.NewAckEventWithBytes(t.observer, message[0:16], message[16:20])) {
				break ReceiverLoop
			}
		default:
			err = fmt.Errorf("Unexpected message code: %s", header[0:4])
			break ReceiverLoop
		}
	}

	if err != nil {
		// Pass the error back and abort
	FailLoop:
		for {
			select {
			case <-t.recvControl:
				// Shutdown
				break FailLoop
			case t.failChan <- err:
			}
		}
	}
}

// receiverRead will repeatedly read from the socket until the given byte array
// is filled.
func (t *TransportTCP) receiverRead(data []byte) (bool, error) {
	received := 0

ReceiverReadLoop:
	for {
		select {
		case <-t.recvControl:
			// Shutdown
			break ReceiverReadLoop
		default:
			// Timeout after socketIntervalSeconds, check for shutdown, and try again
			t.socket.SetReadDeadline(time.Now().Add(socketIntervalSeconds * time.Second))

			length, err := t.socket.Read(data[received:])
			received += length

			if received >= len(data) {
				// Success
				return false, nil
			}

			if err == nil {
				// Keep trying
				continue
			}

			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Keep trying
				continue
			}

			// Pass an error back
			return false, err
		}
	}

	return true, nil
}

// sendEvent ships an event structure to the observer whilst also monitoring for
// any shutdown signal. Returns true if shutdown was signalled
func (t *TransportTCP) sendEvent(controlChan <-chan int, event transports.Event) bool {
	select {
	case <-controlChan:
		return true
	case t.observer.EventChan() <- event:
	}
	return false
}

// Write a message to the transport
func (t *TransportTCP) Write(nonce string, events []*core.EventDescriptor) error {
	var messageBuffer bytes.Buffer

	// Encapsulate the data into the message
	// 4-byte message header (JDAT = JSON Data, Compressed)
	// 4-byte uint32 data length
	// Then the data
	if _, err := messageBuffer.Write([]byte("JDAT")); err != nil {
		return err
	}

	// False length as we don't know it yet
	if _, err := messageBuffer.Write([]byte("----")); err != nil {
		return err
	}

	// Create the compressed data payload
	// 16-byte Nonce, followed by the compressed event data
	// The event data is each event, prefixed with a 4-byte uint32 length, one
	// after the other
	if _, err := messageBuffer.Write([]byte(nonce)); err != nil {
		return err
	}

	compressor, err := zlib.NewWriterLevel(&messageBuffer, 3)
	if err != nil {
		return err
	}

	for _, event := range events {
		if err := binary.Write(compressor, binary.BigEndian, uint32(len(event.Event))); err != nil {
			return err
		}

		if _, err := compressor.Write(event.Event); err != nil {
			return err
		}
	}

	compressor.Close()

	// Fill in the size
	// TODO: This prevents us bypassing buffer and just sending...
	//       New JDA2? With FFFF size? Means stream message?
	messageBytes := messageBuffer.Bytes()
	binary.BigEndian.PutUint32(messageBytes[4:8], uint32(messageBuffer.Len()-8))

	t.sendChan <- messageBytes
	return nil
}

// Ping the remote server
func (t *TransportTCP) Ping() error {
	// Encapsulate the ping into a message
	// 4-byte message header (PING)
	// 4-byte uint32 data length (0 length for PING)
	t.sendChan <- []byte{'P', 'I', 'N', 'G', 0, 0, 0, 0}
	return nil
}

// Fail the transport
func (t *TransportTCP) Fail() {
	t.failChan <- nil
}

// Shutdown the transport
func (t *TransportTCP) Shutdown() {
	close(t.controllerChan)
}
