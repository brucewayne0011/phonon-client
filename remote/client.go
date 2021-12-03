package remote

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"encoding/gob"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GridPlus/phonon-client/card"
	"github.com/GridPlus/phonon-client/cert"
	"github.com/posener/h2conn"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
)

type RemoteConnection struct {
	conn                     *h2conn.Conn
	encoder                  *gob.Encoder
	remoteCertificate        *cert.CardCertificate
	session                  *card.Session
	identifiedWithServerChan chan bool
	identifiedWithServer     bool
	counterpartyNonce        [32]byte
	verified                 bool
	connectedToCardChan      chan bool
	pairFinalized            bool

	//card pairing message channels
	remoteCertificateChan    chan cert.CardCertificate
	remoteIdentityChan       chan []byte
	cardPair1DataChan        chan []byte
	finalizeCardPairDataChan chan []byte

	phononAckChan chan bool
}

// this will go someplace, I swear
var ErrTimeout = errors.New("Timeout")

func Connect(s *card.Session, url string, ignoreTLS bool) (*RemoteConnection, error) {
	d := &h2conn.Client{
		Client: &http.Client{
			Transport: &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: ignoreTLS}},
		},
	}

	conn, _, err := d.Connect(context.Background(), url) //url)
	if err != nil {
		return nil, fmt.Errorf("Unable to connect to remote server %e,", err)
	}
	remoteConn := &RemoteConnection{
		conn: conn,
		//initialize connection channels
		connectedToCardChan:      make(chan bool, 1),
		identifiedWithServerChan: make(chan bool, 1),
		//initialize card pairing channels
		remoteCertificateChan:    make(chan cert.CardCertificate, 1),
		remoteIdentityChan:       make(chan []byte, 1),
		cardPair1DataChan:        make(chan []byte, 1),
		finalizeCardPairDataChan: make(chan []byte, 1),

		phononAckChan: make(chan bool, 1),
	}

	go remoteConn.HandleIncoming()
	remoteConn.encoder = gob.NewEncoder(conn)
	remoteConn.session = s
	return remoteConn, nil
}

// memory leak ohh boy!
func (c *RemoteConnection) HandleIncoming() {
	cmdDecoder := gob.NewDecoder(c.conn)
	messageChan := make(chan (Message))

	go func(msgchan chan Message) {
		defer close(msgchan)
		for {
			message := Message{}
			//todo read raw and decode separately to avoid killing the whole thing on a malformed message
			err := cmdDecoder.Decode(&message)
			if err != nil {
				log.Info("Error receiving message from connected server")
				return
			}
			msgchan <- message
		}
	}(messageChan)

	for message := range messageChan {
		c.process(message)
	}
}

func (c *RemoteConnection) process(msg Message) {
	switch msg.Name {
	case RequestCertificate:
		c.sendCertificate(msg)
	case ResponseCertificate:
		c.receiveCertificate(msg)
	case RequestIdentify:
		c.sendIdentify(msg)
	case ResponseIdentify:
		c.processIdentify(msg)
	case MessageError:
		log.Error(string(msg.Payload))
	case MessageIdentifiedWithServer:
		c.identifiedWithServerChan <- true
		c.identifiedWithServer = true
	case MessageConnectedToCard:
		c.connectedToCardChan <- true
	// Card pairing requests and responses
	case RequestCardPair1:
		c.processCardPair1(msg)
	case ResponseCardPair1:
		c.cardPair1DataChan <- msg.Payload
	case RequestFinalizeCardPair:
		c.processFinalizeCardPair(msg)
	case ResponseFinalizeCardPair:
		c.finalizeCardPairDataChan <- msg.Payload
	case MessagePhononAck:
		c.phononAckChan <- true
	case RequestReceivePhonon:
		c.processReceivePhonons(msg)
	}
}

/////
// Below are the request processing methods
/////

func (c *RemoteConnection) sendCertificate(msg Message) {
	cert, err := c.session.GetCertificate()
	if err != nil {
		log.Error("Cert doesn't exist")
	}
	c.sendMessage(ResponseCertificate, cert.Serialize())
}

func (c *RemoteConnection) sendIdentify(msg Message) {
	_, sig, err := c.session.IdentifyCard(msg.Payload)
	if err != nil {
		log.Error("Issue identifying local card", err.Error())
		return
	}
	payload := []byte{}
	buf := bytes.NewBuffer(payload)
	enc := gob.NewEncoder(buf)
	enc.Encode(sig)
	c.sendMessage(ResponseIdentify, buf.Bytes())
}

func (c *RemoteConnection) processIdentify(msg Message) {
	key, sig, err := card.ParseIdentifyCardResponse(msg.Payload)
	if err != nil {
		log.Error("Issue parsing identify card response", err.Error())
		return
	}
	if !ecdsa.Verify(key, c.counterpartyNonce[:], sig.R, sig.S) {
		log.Error("Unable to verify card challenge")
		return
	} else {
		c.verified = true
		return
	}
}

func (c *RemoteConnection) processCardPair1(msg Message) {
	cardPairData, err := c.session.CardPair(msg.Payload)
	if err != nil {
		log.Error("error with card pair 1", err.Error())
	}
	c.sendMessage(ResponseCardPair1, cardPairData)

}

func (c *RemoteConnection) processFinalizeCardPair(msg Message) {
	err := c.session.FinalizeCardPair(msg.Payload)
	if err != nil {
		log.Error("Error finalizing Card Pair", err.Error())
		c.sendMessage(ResponseFinalizeCardPair, []byte(err.Error()))
		return
	}
	c.sendMessage(ResponseFinalizeCardPair, []byte{})
	c.pairFinalized = true
	c.session.RemoteCard = c
	//c.finalizeCardPairErrorChan <- err
}

func (c *RemoteConnection) processReceivePhonons(msg Message) {
	err := c.session.ReceivePhonons(msg.Payload)
	if err != nil {
		log.Error(err.Error())
		return
	}
	c.sendMessage(MessagePhononAck, []byte{})
}

// ProcessProvideCertificate is for adding a remote card's certificate to the remote portion of the struct
func (c *RemoteConnection) receiveCertificate(msg Message) {
	remoteCert, err := cert.ParseRawCardCertificate(msg.Payload)
	if err != nil {
		log.Error(err)
		return
	}
	c.remoteCertificateChan <- remoteCert
	c.remoteCertificate = &remoteCert
}

/////
// Below are the methods that satisfy the interface for remote counterparty
/////
func (c *RemoteConnection) Identify() error {
	var nonce [32]byte
	rand.Read(nonce[:])
	c.counterpartyNonce = nonce
	c.sendMessage(RequestIdentify, nonce[:])
	select {
	case <-c.remoteIdentityChan:
		return nil
	case <-time.After(10 * time.Second):
		return ErrTimeout

	}
}

func (c *RemoteConnection) CardPair(initPairingData []byte) (cardPairData []byte, err error) {
	c.sendMessage(RequestCardPair1, initPairingData)
	select {
	case cardPairData := <-c.cardPair1DataChan:
		return cardPairData, nil
	case <-time.After(10 * time.Second):
		return []byte{}, ErrTimeout
	}
}

func (c *RemoteConnection) CardPair2(cardPairData []byte) (cardPairData2 []byte, err error) {
	//unneeded
	return []byte{}, nil
}

func (c *RemoteConnection) FinalizeCardPair(cardPair2Data []byte) error {
	c.sendMessage(RequestFinalizeCardPair, cardPair2Data)
	if !c.pairFinalized {
		select {
		case errorbytes := <-c.finalizeCardPairDataChan:
			var err error
			if len(errorbytes) > 0 {
				return errors.New(string(errorbytes))
			} else {
				return err
			}
		case <-time.After(10 * time.Second):
			return ErrTimeout
		}
	}
	c.pairFinalized = true
	c.session.RemoteCard = c
	return nil
}

func (c *RemoteConnection) GetCertificate() (*cert.CardCertificate, error) {
	if c.remoteCertificate == nil {
		c.sendMessage(RequestCertificate, []byte{})
		select {
		case cert := <-c.remoteCertificateChan:
			c.remoteCertificate = &cert
		case <-time.After(10 * time.Second):
			return nil, ErrTimeout
		}

	}
	return c.remoteCertificate, nil
}

func (c *RemoteConnection) ConnectToCard(cardID string) error {
	if !c.identifiedWithServer {
		select {
		case <-time.After(10 * time.Second):
			return ErrTimeout
		case <-c.identifiedWithServerChan:
			log.Info("received Identified with server")
		}
	}
	log.Info("sending requestConnectCard2Card message")
	c.sendMessage(RequestConnectCard2Card, []byte(cardID))
	select {
	case <-time.After(10 * time.Second):
		log.Error("Connection Timed out Waiting for peer")
		c.conn.Close()
		return ErrTimeout
	case <-c.connectedToCardChan:
		return nil
	}
}

func (c *RemoteConnection) ReceivePhonons(PhononTransfer []byte) error {
	c.sendMessage(RequestReceivePhonon, PhononTransfer)
	select {
	case <-time.After(10 * time.Second):
		log.Error("unable to verify remote recipt of phonons")
		return ErrTimeout
	case <-c.phononAckChan:
		return nil
	}
}

func (c *RemoteConnection) GenerateInvoice() (invoiceData []byte, err error) {
	// todo:
	return
}

func (c *RemoteConnection) ReceiveInvoice(invoiceData []byte) error {
	// todo:
	return nil
}

// Utility functions
func (c *RemoteConnection) sendMessage(messageName string, messagePayload []byte) {
	log.Debug(messageName, string(messagePayload))

	tosend := &Message{
		Name:    messageName,
		Payload: messagePayload,
	}

	c.encoder.Encode(tosend)
}