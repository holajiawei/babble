package wamp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/gammazero/nexus/v3/client"
	"github.com/gammazero/nexus/v3/wamp"
	"github.com/mosaicnetworks/babble/src/net/signal"
	"github.com/pion/webrtc/v2"
	"github.com/sirupsen/logrus"
)

// Client implements the Signal interface. It sends and receives SDP offers
// through a WAMP server using WebSockets.
type Client struct {
	pubKey   string
	client   *client.Client
	consumer chan signal.OfferPromise
	logger   *logrus.Entry
}

// NewClient instantiates a new Client, and opens a connection to the WAMP
// signaling server.
func NewClient(server string,
	realm string,
	pubKey string,
	caFile string,
	insecureSkipVerify bool,
	logger *logrus.Entry) (*Client, error) {

	cfg := client.Config{
		Realm:  realm,
		Logger: logger,
	}

	tlscfg := &tls.Config{}

	if insecureSkipVerify {
		logger.Debug("Skip Verify. Accepting any certificate provided by signal server.")
		tlscfg.InsecureSkipVerify = true
	} else if _, err := os.Stat(caFile); os.IsNotExist(err) {
		logger.Debugf("No certificate file found. Relying on platform trusted certificates.")
	} else {
		// Load PEM-encoded certificate to trust.
		certPEM, err := ioutil.ReadFile(caFile)
		if err != nil {
			return nil, err
		}

		// Create CertPool containing the certificate to trust.
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(certPEM) {
			return nil, errors.New("Failed to import certificate to trust")
		}

		// Trust the certificate by putting it into the pool of root CAs.
		tlscfg.RootCAs = roots

		// Decode and parse the server cert to extract the subject info.
		block, _ := pem.Decode(certPEM)
		if block == nil {

			return nil, errors.New("Failed to decode certificate to trust")
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}

		logger.Debugf("Trusting certificate %s with CN: %s", caFile, cert.Subject.CommonName)

		// Set ServerName in TLS config to CN from trusted cert so that
		// certificate will validate if CN does not match DNS name.
		tlscfg.ServerName = cert.Subject.CommonName
	}

	cfg.TlsCfg = tlscfg

	cli, err := client.ConnectNet(
		context.Background(),
		fmt.Sprintf("wss://%s", server),
		cfg,
	)
	if err != nil {
		return nil, err
	}

	res := &Client{
		pubKey:   pubKey,
		client:   cli,
		consumer: make(chan signal.OfferPromise),
		logger:   logger,
	}

	return res, nil
}

// ID implements the Signal interface. It returns the pubKey indentifying this
// client
func (c *Client) ID() string {
	return c.pubKey
}

// Listen implements the Signal interface. It registers a callback within the
// WAMP router. The callback forwards offers to the consumer channel. The
// callback is identified by the client's public key.
func (c *Client) Listen() error {
	if err := c.client.Register(c.ID(), c.callHandler, nil); err != nil {
		c.logger.WithError(err).Error("Failed to register procedure")
		return err
	}
	c.logger.Debug("Registered procedure with router")
	return nil
}

// Offer implements the Signal interface. It sends an offer and waits for an
// answer.
func (c *Client) Offer(target string, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	raw, err := json.Marshal(offer)
	if err != nil {
		return nil, err
	}

	// TODO formalise RPC args
	callArgs := wamp.List{
		string(c.pubKey),
		string(raw),
	}

	result, err := c.client.Call(context.Background(), target, nil, callArgs, nil, nil)
	if err != nil {
		c.logger.Error(err)
		return nil, err
	}

	// TODO formalise RPC args
	sdp, ok := wamp.AsString(result.Arguments[0])
	if !ok {
		return nil, err
	}

	answer := webrtc.SessionDescription{}
	err = json.Unmarshal([]byte(sdp), &answer)
	if err != nil {
		return nil, err
	}

	return &answer, nil

}

// Consumer implements the Signal interface. It returns the channel through
// which incoming WebRTC offers are received. The offers are wrapped insided
// promises which provide an asynchronous response mechanism.
func (c *Client) Consumer() <-chan signal.OfferPromise {
	return c.consumer
}

// Close closes the connection to the WAMP server
func (c *Client) Close() error {
	c.client.Unregister(c.ID())
	return c.client.Close()
}

// TODO formalise RPC arguments and errors
func (c *Client) callHandler(ctx context.Context, inv *wamp.Invocation) client.InvokeResult {
	if len(inv.Arguments) != 2 {
		return errResult(
			fmt.Sprintf("Invocation should contain 2 arguments, not %d", len(inv.Arguments)))
	}

	from, ok := wamp.AsString(inv.Arguments[0])
	if !ok {
		return errResult("Error reading invocation first argument")
	}

	sdp, ok := wamp.AsString(inv.Arguments[1])
	if !ok {
		return errResult("Error reading invocation second argument")
	}

	offer := webrtc.SessionDescription{}
	err := json.Unmarshal([]byte(sdp), &offer)
	if err != nil {
		return errResult(fmt.Sprintf("Error parsing invocation SDP: %v", err))
	}

	respCh := make(chan signal.OfferPromiseResponse, 1)

	promise := signal.OfferPromise{
		From:     from,
		Offer:    offer,
		RespChan: respCh,
	}

	c.consumer <- promise

	// Wait for response
	// TODO Timeout?
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return errResult(resp.Error.Error())
		}

		raw, err := json.Marshal(resp.Answer)
		if err != nil {
			return errResult(fmt.Sprintf("Error parsing answer: %v", err))
		}

		return client.InvokeResult{
			Args: wamp.List{string(raw)},
		}
	}
}

func errResult(msg string) client.InvokeResult {
	return client.InvokeResult{
		Err:  ErrProcessingOffer,
		Args: wamp.List{msg},
	}
}
