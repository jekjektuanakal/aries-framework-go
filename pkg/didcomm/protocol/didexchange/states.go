/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package didexchange

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mitchellh/mapstructure"

	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/model"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/service"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/decorator"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/protocol/mediator"
	"github.com/hyperledger/aries-framework-go/pkg/doc/did"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/suite"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/suite/ed25519signature2018"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/verifier"
	vdrapi "github.com/hyperledger/aries-framework-go/pkg/framework/aries/api/vdr"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/hyperledger/aries-framework-go/pkg/kms/localkms"
	connectionstore "github.com/hyperledger/aries-framework-go/pkg/store/connection"
	"github.com/hyperledger/aries-framework-go/pkg/vdr/fingerprint"
	"github.com/hyperledger/aries-framework-go/spi/storage"
)

const (
	stateNameNoop = "noop"
	stateNameNull = "null"
	// StateIDInvited marks the invited phase of the did-exchange protocol.
	StateIDInvited = "invited"
	// StateIDRequested marks the requested phase of the did-exchange protocol.
	StateIDRequested = "requested"
	// StateIDResponded marks the responded phase of the did-exchange protocol.
	StateIDResponded = "responded"
	// StateIDCompleted marks the completed phase of the did-exchange protocol.
	StateIDCompleted = "completed"
	// StateIDAbandoned marks the abandoned phase of the did-exchange protocol.
	StateIDAbandoned           = "abandoned"
	ackStatusOK                = "ok"
	didCommServiceType         = "did-communication"
	timestamplen               = 8
	ed25519VerificationKey2018 = "Ed25519VerificationKey2018"
	bls12381G2Key2020          = "Bls12381G2Key2020"
	jsonWebKey2020             = "JsonWebKey2020"
	didMethod                  = "peer"
)

var errVerKeyNotFound = errors.New("verkey not found")

// state action for network call.
type stateAction func() error

// The did-exchange protocol's state.
type state interface {
	// Name of this state.
	Name() string

	// Whether this state allows transitioning into the next state.
	CanTransitionTo(next state) bool

	// ExecuteInbound this state, returning a followup state to be immediately executed as well.
	// The 'noOp' state should be returned if the state has no followup.
	ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (connRecord *connectionstore.Record,
		state state, action stateAction, err error)
}

// Returns the state towards which the protocol will transition to if the msgType is processed.
func stateFromMsgType(msgType string) (state, error) {
	switch msgType {
	case InvitationMsgType, oobMsgType:
		return &invited{}, nil
	case RequestMsgType:
		return &requested{}, nil
	case ResponseMsgType:
		return &responded{}, nil
	case AckMsgType, CompleteMsgType:
		return &completed{}, nil
	default:
		return nil, fmt.Errorf("unrecognized msgType: %s", msgType)
	}
}

// Returns the state representing the name.
func stateFromName(name string) (state, error) {
	switch name {
	case stateNameNoop:
		return &noOp{}, nil
	case stateNameNull:
		return &null{}, nil
	case StateIDInvited:
		return &invited{}, nil
	case StateIDRequested:
		return &requested{}, nil
	case StateIDResponded:
		return &responded{}, nil
	case StateIDCompleted:
		return &completed{}, nil
	case StateIDAbandoned:
		return &abandoned{}, nil
	default:
		return nil, fmt.Errorf("invalid state name %s", name)
	}
}

type noOp struct{}

func (s *noOp) Name() string {
	return stateNameNoop
}

func (s *noOp) CanTransitionTo(_ state) bool {
	return false
}

func (s *noOp) ExecuteInbound(_ *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	return nil, nil, nil, errors.New("cannot execute no-op")
}

// null state.
type null struct{}

func (s *null) Name() string {
	return stateNameNull
}

func (s *null) CanTransitionTo(next state) bool {
	return StateIDInvited == next.Name() || StateIDRequested == next.Name()
}

func (s *null) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	return &connectionstore.Record{}, &noOp{}, nil, nil
}

// invited state.
type invited struct{}

func (s *invited) Name() string {
	return StateIDInvited
}

func (s *invited) CanTransitionTo(next state) bool {
	return StateIDRequested == next.Name()
}

func (s *invited) ExecuteInbound(msg *stateMachineMsg, _ string, _ *context) (*connectionstore.Record,
	state, stateAction, error) {
	if msg.Type() != InvitationMsgType && msg.Type() != oobMsgType {
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}

	return msg.connRecord, &requested{}, func() error { return nil }, nil
}

// requested state.
type requested struct{}

func (s *requested) Name() string {
	return StateIDRequested
}

func (s *requested) CanTransitionTo(next state) bool {
	return StateIDResponded == next.Name()
}

func (s *requested) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	switch msg.Type() {
	case oobMsgType:
		action, record, err := ctx.handleInboundOOBInvitation(msg, thid, msg.options)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to handle inbound oob invitation : %w", err)
		}

		return record, &noOp{}, action, nil
	case InvitationMsgType:
		invitation := &Invitation{}

		err := msg.Decode(invitation)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of invitation: %w", err)
		}

		action, connRecord, err := ctx.handleInboundInvitation(invitation, thid, msg.options, msg.connRecord)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("handle inbound invitation: %w", err)
		}

		return connRecord, &noOp{}, action, nil
	case RequestMsgType:
		return msg.connRecord, &responded{}, func() error { return nil }, nil
	default:
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}
}

// responded state.
type responded struct{}

func (s *responded) Name() string {
	return StateIDResponded
}

func (s *responded) CanTransitionTo(next state) bool {
	return StateIDCompleted == next.Name()
}

func (s *responded) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	switch msg.Type() {
	case RequestMsgType:
		request := &Request{}

		err := msg.Decode(request)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of request: %w", err)
		}

		action, connRecord, err := ctx.handleInboundRequest(request, msg.options, msg.connRecord)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("handle inbound request: %w", err)
		}

		return connRecord, &noOp{}, action, nil
	case ResponseMsgType, CompleteMsgType:
		return msg.connRecord, &completed{}, func() error { return nil }, nil
	default:
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}
}

// completed state.
type completed struct{}

func (s *completed) Name() string {
	return StateIDCompleted
}

func (s *completed) CanTransitionTo(next state) bool {
	return false
}

func (s *completed) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	switch msg.Type() {
	case ResponseMsgType:
		response := &Response{}

		err := msg.Decode(response)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of response: %w", err)
		}

		action, connRecord, err := ctx.handleInboundResponse(response)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("handle inbound response: %w", err)
		}

		return connRecord, &noOp{}, action, nil
	case AckMsgType:
		action := func() error { return nil }
		return msg.connRecord, &noOp{}, action, nil
	case CompleteMsgType:
		complete := &Complete{}

		err := msg.Decode(complete)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("JSON unmarshalling of complete: %w", err)
		}

		action := func() error { return nil }

		if msg.connRecord == nil {
			return nil, &noOp{}, action, nil
		}

		connRec := *msg.connRecord

		return &connRec, &noOp{}, action, nil
	default:
		return nil, nil, nil, fmt.Errorf("illegal msg type %s for state %s", msg.Type(), s.Name())
	}
}

// abandoned state.
type abandoned struct{}

func (s *abandoned) Name() string {
	return StateIDAbandoned
}

func (s *abandoned) CanTransitionTo(next state) bool {
	return false
}

func (s *abandoned) ExecuteInbound(msg *stateMachineMsg, thid string, ctx *context) (*connectionstore.Record,
	state, stateAction, error) {
	return nil, nil, nil, errors.New("not implemented")
}

func (ctx *context) handleInboundOOBInvitation(
	msg *stateMachineMsg, thid string, options *options) (stateAction, *connectionstore.Record, error) {
	myDID, conn, err := ctx.getDIDDocAndConnection(getPublicDID(options), getRouterConnections(options))
	if err != nil {
		return nil, nil, fmt.Errorf("handleInboundOOBInvitation - failed to get diddoc and connection: %w", err)
	}

	msg.connRecord.MyDID = myDID.ID
	msg.connRecord.ThreadID = thid

	oobInvitation := OOBInvitation{}

	err = msg.Decode(&oobInvitation)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode oob invitation: %w", err)
	}

	request := &Request{
		Type:       RequestMsgType,
		ID:         thid,
		Label:      oobInvitation.MyLabel,
		Connection: conn,
		Thread: &decorator.Thread{
			ID:  thid,
			PID: msg.connRecord.ParentThreadID,
		},
	}

	svc, err := ctx.getServiceBlock(&oobInvitation)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get service block: %w", err)
	}

	dest := &service.Destination{
		RecipientKeys:     svc.RecipientKeys,
		ServiceEndpoint:   svc.ServiceEndpoint,
		RoutingKeys:       svc.RoutingKeys,
		MediaTypeProfiles: svc.Accept,
	}

	recipientKey, err := recipientKey(myDID)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound OOBInvitation: %w", err)
	}

	return func() error {
		logger.Debugf("dispatching outbound request on thread: %+v", request.Thread)
		return ctx.outboundDispatcher.Send(request, recipientKey, dest)
	}, msg.connRecord, nil
}

func (ctx *context) handleInboundInvitation(invitation *Invitation, thid string, options *options,
	connRec *connectionstore.Record) (stateAction, *connectionstore.Record, error) {
	// create a destination from invitation
	destination, err := ctx.getDestination(invitation)
	if err != nil {
		return nil, nil, err
	}

	// get did document that will be used in exchange request
	didDoc, conn, err := ctx.getDIDDocAndConnection(getPublicDID(options), getRouterConnections(options))
	if err != nil {
		return nil, nil, err
	}

	pid := invitation.ID
	if connRec.Implicit {
		pid = invitation.DID
	}

	request := &Request{
		Type:       RequestMsgType,
		ID:         thid,
		Label:      getLabel(options),
		Connection: conn,
		Thread: &decorator.Thread{
			PID: pid,
		},
	}
	connRec.MyDID = request.Connection.DID

	senderKey, err := recipientKey(didDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound invitation: %w", err)
	}

	return func() error {
		return ctx.outboundDispatcher.Send(request, senderKey, destination)
	}, connRec, nil
}

func (ctx *context) handleInboundRequest(request *Request, options *options,
	connRec *connectionstore.Record) (stateAction, *connectionstore.Record, error) {
	logger.Debugf("handling request: %+v", request)

	requestConnection, err := getRequestConnection(request)
	if err != nil {
		return nil, nil, fmt.Errorf("extracting connection data from request: %w", err)
	}

	requestDidDoc, err := ctx.resolveDidDocFromConnection(requestConnection)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve did doc from exchange request connection: %w", err)
	}

	// get did document that will be used in exchange response
	// (my did doc)
	responseDidDoc, responseConnection, err := ctx.getDIDDocAndConnection(
		getPublicDID(options), getRouterConnections(options))
	if err != nil {
		return nil, nil, fmt.Errorf("get response did doc and connection: %w", err)
	}

	senderVerKey, err := recipientKey(responseDidDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound request: %w", err)
	}

	if ctx.doACAPyInterop {
		// Interop: aca-py issue https://github.com/hyperledger/aries-cloudagent-python/issues/1048
		responseDidDoc, err = convertPeerToSov(responseDidDoc)
		if err != nil {
			return nil, nil, fmt.Errorf("converting my did doc to a 'sov' doc for response message: %w", err)
		}
	}

	response, err := ctx.prepareResponse(request, responseDidDoc, responseConnection)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing response: %w", err)
	}

	connRec.TheirDID = requestConnection.DID
	connRec.MyDID = responseConnection.DID
	connRec.TheirLabel = request.Label

	destination, err := service.CreateDestination(requestDidDoc)
	if err != nil {
		return nil, nil, err
	}

	if len(destination.MediaTypeProfiles) > 0 {
		connRec.MediaTypeProfiles = destination.MediaTypeProfiles
	}

	// send exchange response
	return func() error {
		return ctx.outboundDispatcher.Send(response, senderVerKey, destination)
	}, connRec, nil
}

func (ctx *context) prepareResponse(request *Request, responseDidDoc *did.Doc,
	responseConnection *Connection) (*Response, error) {
	// prepare the response
	response := &Response{
		Type: ResponseMsgType,
		ID:   uuid.New().String(),
		Thread: &decorator.Thread{
			ID: request.ID,
		},
	}

	if request.Thread != nil {
		response.Thread.PID = request.Thread.PID
	}

	// TODO followup: bring this out from under doACAPYInterop
	//  and remove legacy usage of connection/connectionSignature.
	//  part of issue https://github.com/hyperledger/aries-framework-go/issues/2495
	if ctx.doACAPyInterop {
		return ctx.prepareResponseWithSignedAttachment(request, response, responseDidDoc)
	}

	// prepare connection signature
	encodedConnectionSignature, err := ctx.prepareConnectionSignature(responseConnection, request.Thread.PID)
	if err != nil {
		return nil, fmt.Errorf("connection signature: %w", err)
	}

	response.ConnectionSignature = encodedConnectionSignature

	return response, nil
}

func (ctx *context) prepareResponseWithSignedAttachment(request *Request, response *Response,
	responseDidDoc *did.Doc) (*Response, error) {
	if request.DocAttach != nil {
		err := request.DocAttach.Data.Verify(ctx.crypto, ctx.kms)
		if err != nil {
			return nil, fmt.Errorf("verifying signature on doc~attach: %w", err)
		}
	}

	var docBytes []byte

	var err error

	if ctx.doACAPyInterop {
		docBytes, err = responseDidDoc.SerializeInterop()
		if err != nil {
			return nil, fmt.Errorf("marshaling did doc: %w", err)
		}
	}

	docAttach := &decorator.Attachment{
		MimeType: "application/json",
		Data: decorator.AttachmentData{
			Base64: base64.StdEncoding.EncodeToString(docBytes),
		},
	}

	invitationKey, err := ctx.getVerKey(request.Thread.PID)
	if err != nil {
		return nil, fmt.Errorf("getting sender verkey: %w", err)
	}

	pubKeyBytes, err := fingerprint.PubKeyFromDIDKey(invitationKey)
	if err != nil {
		return nil, fmt.Errorf("failed to extract pubKeyBytes from did:key [%s]: %w", invitationKey, err)
	}

	// TODO: use dynamic context KeyType
	signingKID, err := localkms.CreateKID(pubKeyBytes, kms.ED25519Type)
	if err != nil {
		return nil, fmt.Errorf("failed to generate KID from public key: %w", err)
	}

	kh, err := ctx.kms.Get(signingKID)
	if err != nil {
		return nil, fmt.Errorf("failed to get key handle: %w", err)
	}

	err = docAttach.Data.Sign(ctx.crypto, kh, ed25519.PublicKey(pubKeyBytes), pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("signing did_doc~attach: %w", err)
	}

	// Interop: aca-py expects naked DID method-specific identifier for sov DIDs
	// https://github.com/hyperledger/aries-cloudagent-python/issues/1048
	response.DID = strings.TrimPrefix(responseDidDoc.ID, "did:sov:")
	response.DocAttach = docAttach

	return response, nil
}

func getPublicDID(options *options) string {
	if options == nil {
		return ""
	}

	return options.publicDID
}

func getRouterConnections(options *options) []string {
	if options == nil {
		return nil
	}

	return options.routerConnections
}

// returns the label given in the options, otherwise an empty string.
func getLabel(options *options) string {
	if options == nil {
		return ""
	}

	return options.label
}

func (ctx *context) getDestination(invitation *Invitation) (*service.Destination, error) {
	if invitation.DID != "" {
		return service.GetDestination(invitation.DID, ctx.vdRegistry)
	}

	return &service.Destination{
		RecipientKeys:   invitation.RecipientKeys,
		ServiceEndpoint: invitation.ServiceEndpoint,
		RoutingKeys:     invitation.RoutingKeys,
	}, nil
}

// nolint:gocyclo,funlen
func (ctx *context) getDIDDocAndConnection(pubDID string, routerConnections []string) (*did.Doc, *Connection, error) {
	if pubDID != "" {
		logger.Debugf("using public did[%s] for connection", pubDID)

		docResolution, err := ctx.vdRegistry.Resolve(pubDID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve public did[%s]: %w", pubDID, err)
		}

		err = ctx.connectionStore.SaveDIDFromDoc(docResolution.DIDDocument)
		if err != nil {
			return nil, nil, err
		}

		return docResolution.DIDDocument, &Connection{DID: docResolution.DIDDocument.ID}, nil
	}

	logger.Debugf("creating new '%s' did for connection", didMethod)

	var services []did.Service

	for _, connID := range routerConnections {
		// get the route configs (pass empty service endpoint, as default service endpoint added in VDR)
		serviceEndpoint, routingKeys, err := mediator.GetRouterConfig(ctx.routeSvc, connID, "")
		if err != nil {
			return nil, nil, fmt.Errorf("did doc - fetch router config: %w", err)
		}

		services = append(services, did.Service{ServiceEndpoint: serviceEndpoint, RoutingKeys: routingKeys})
	}

	if len(services) == 0 {
		services = append(services, did.Service{})
	}

	newDID := &did.Doc{Service: services}

	err := createNewKeyAndVerificationMethod(newDID, kms.ED25519, ctx.kms)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create and export public key: %w", err)
	}

	// by default use peer did
	docResolution, err := ctx.vdRegistry.Create(didMethod, newDID)
	if err != nil {
		return nil, nil, fmt.Errorf("create %s did: %w", didMethod, err)
	}

	if len(routerConnections) != 0 {
		svc, ok := did.LookupService(docResolution.DIDDocument, didCommServiceType)
		if ok {
			for _, recKey := range svc.RecipientKeys {
				for _, connID := range routerConnections {
					// TODO https://github.com/hyperledger/aries-framework-go/issues/1105 Support to Add multiple
					//  recKeys to the Router
					if err = mediator.AddKeyToRouter(ctx.routeSvc, connID, recKey); err != nil {
						return nil, nil, fmt.Errorf("did doc - add key to the router: %w", err)
					}
				}
			}
		}
	}

	err = ctx.connectionStore.SaveDIDFromDoc(docResolution.DIDDocument)
	if err != nil {
		return nil, nil, err
	}

	connection := &Connection{
		DID:    docResolution.DIDDocument.ID,
		DIDDoc: docResolution.DIDDocument,
	}

	return docResolution.DIDDocument, connection, nil
}

func createNewKeyAndVerificationMethod(didDoc *did.Doc, keyType kms.KeyType, keyManager kms.KeyManager) error {
	vmType := getVerMethodType(keyType)

	kid, pubKeyBytes, err := keyManager.CreateAndExportPubKeyBytes(keyType)
	if err != nil {
		return err
	}

	pubKeyBytes, err = convertPubKeyBytes(pubKeyBytes, keyType)
	if err != nil {
		return err
	}

	vm := did.VerificationMethod{
		ID:    "#" + kid,
		Type:  vmType,
		Value: pubKeyBytes,
	}

	didDoc.VerificationMethod = append(didDoc.VerificationMethod, vm)

	// TODO replace below authentication with a KeyAgreement method when/if DID attachment doesn't require to be signed
	//  as per https://github.com/hyperledger/aries-rfcs/pull/626. If the PR is declined, then only remove this comment.
	//  KeyAgreement is needed for envelope encryption regardless. It must be added in a future change.
	didDoc.Authentication = append(didDoc.Authentication, *did.NewReferencedVerification(&vm, did.Authentication))

	return nil
}

/* func extractKeyTypeFromOpts(createDIDOpts *vdrapi.DIDMethodOpts) (kms.KeyType, error) {
	var keyType kms.KeyType

	k := createDIDOpts.Values[keyvdr.KeyType]
	if k != nil {
		var ok bool
		keyType, ok = k.(kms.KeyType)

		if !ok {
			return "", errors.New("keyType is not kms.KeyType")
		}

		return keyType, nil
	}

	return "", errors.New("keyType option is needed for empty didDoc.VerificationMethod")
} */

// convertPubKeyBytes converts marshalled bytes into 'did:key' ready to use public key bytes. This is mainly useful for
// ECDSA keys. The elliptic.Marshal() function returns 65 bytes (for P-256 curves) where the first byte is the
// compression point, it must be truncated to build a proper public key for did:key construction. It must be set back
// when building a public key from did:key (using default compression point value is 4 for non compressed marshalling,
// see: https://github.com/golang/go/blob/master/src/crypto/elliptic/elliptic.go#L319).
func convertPubKeyBytes(bytes []byte, keyType kms.KeyType) ([]byte, error) {
	switch keyType {
	case kms.ED25519Type, kms.BLS12381G2Type: // no conversion needed for non ECDSA keys.
		return bytes, nil
	case kms.ECDSAP256TypeIEEEP1363, kms.ECDSAP384TypeIEEEP1363, kms.ECDSAP521TypeIEEEP1363:
		// truncate first byte to remove compression point.
		return bytes[1:], nil
	case kms.ECDSAP256TypeDER, kms.ECDSAP384TypeDER, kms.ECDSAP521TypeDER:
		pubKey, err := x509.ParsePKIXPublicKey(bytes)
		if err != nil {
			return nil, err
		}

		ecKey, ok := pubKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, errors.New("invalid EC key")
		}

		// truncate first byte to remove compression point.
		return elliptic.Marshal(ecKey.Curve, ecKey.X, ecKey.Y)[1:], nil
	default:
		return nil, errors.New("invalid key type")
	}
}

// nolint:gochecknoglobals
var vmType = map[kms.KeyType]string{
	kms.ED25519Type:            ed25519VerificationKey2018,
	kms.BLS12381G2Type:         bls12381G2Key2020,
	kms.ECDSAP256TypeDER:       jsonWebKey2020,
	kms.ECDSAP256TypeIEEEP1363: jsonWebKey2020,
	kms.ECDSAP384TypeDER:       jsonWebKey2020,
	kms.ECDSAP384TypeIEEEP1363: jsonWebKey2020,
	kms.ECDSAP521TypeDER:       jsonWebKey2020,
	kms.ECDSAP521TypeIEEEP1363: jsonWebKey2020,
}

func getVerMethodType(kt kms.KeyType) string {
	return vmType[kt]
}

func (ctx *context) resolveDidDocFromConnection(conn *Connection) (*did.Doc, error) {
	didDoc := conn.DIDDoc
	if didDoc == nil {
		// did content was not provided; resolve
		docResolution, err := ctx.vdRegistry.Resolve(conn.DID)
		if err != nil {
			return nil, err
		}

		return docResolution.DIDDocument, err
	}

	id, err := did.Parse(didDoc.ID)
	if err != nil {
		return nil, fmt.Errorf("resolveDidDocFromConnection: failed to parse DID [%s]: %w", didDoc.ID, err)
	}

	// Interop: accommodate aca-py issue https://github.com/hyperledger/aries-cloudagent-python/issues/1048
	method := id.Method
	if method == "sov" {
		method = "peer"
	}

	// store provided did document
	_, err = ctx.vdRegistry.Create(method, didDoc, vdrapi.WithOption("store", true))
	if err != nil {
		return nil, fmt.Errorf("failed to store provided did document: %w", err)
	}

	return didDoc, nil
}

// Encode the connection and convert to Connection Signature as per the spec:
// https://github.com/hyperledger/aries-rfcs/tree/master/features/0023-did-exchange
func (ctx *context) prepareConnectionSignature(connection *Connection,
	invitationID string) (*ConnectionSignature, error) {
	logger.Debugf("connection=%+v invitationID=%s", connection, invitationID)

	connAttributeBytes, err := json.Marshal(connection)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal connection: %w", err)
	}

	now := getEpochTime()
	timestampBuf := make([]byte, timestamplen)
	binary.BigEndian.PutUint64(timestampBuf, uint64(now))
	concatenateSignData := append(timestampBuf, connAttributeBytes...)

	didKey, err := ctx.getVerKey(invitationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get verkey: %w", err)
	}

	pubKeyBytes, err := fingerprint.PubKeyFromDIDKey(didKey)
	if err != nil {
		return nil, fmt.Errorf("failed to extract pubKeyBytes from did:key [%s]: %w", didKey, err)
	}

	signingKID, err := localkms.CreateKID(pubKeyBytes, kms.ED25519Type)
	if err != nil {
		return nil, fmt.Errorf("prepareConnectionSignature: failed to generate KID from public key: %w", err)
	}

	kh, err := ctx.kms.Get(signingKID)
	if err != nil {
		return nil, fmt.Errorf("prepareConnectionSignature: failed to get key handle: %w", err)
	}

	// TODO: Replace with signed attachments issue-626
	signature, err := ctx.crypto.Sign(concatenateSignData, kh)
	if err != nil {
		return nil, fmt.Errorf("sign response message: %w", err)
	}

	return &ConnectionSignature{
		Type:       "https://didcomm.org/signature/1.0/ed25519Sha512_single",
		SignedData: base64.URLEncoding.EncodeToString(concatenateSignData),
		SignVerKey: didKey,
		Signature:  base64.URLEncoding.EncodeToString(signature),
	}, nil
}

func (ctx *context) handleInboundResponse(response *Response) (stateAction, *connectionstore.Record, error) {
	ack := &model.Ack{
		Type:   AckMsgType,
		ID:     uuid.New().String(),
		Status: ackStatusOK,
		Thread: &decorator.Thread{
			ID: response.Thread.ID,
		},
	}

	nsThID, err := connectionstore.CreateNamespaceKey(myNSPrefix, ack.Thread.ID)
	if err != nil {
		return nil, nil, err
	}

	connRecord, err := ctx.connectionRecorder.GetConnectionRecordByNSThreadID(nsThID)
	if err != nil {
		return nil, nil, fmt.Errorf("get connection record: %w", err)
	}

	conn, err := verifySignature(response.ConnectionSignature, connRecord.RecipientKeys[0])
	if err != nil {
		return nil, nil, err
	}

	connRecord.TheirDID = conn.DID

	responseDidDoc, err := ctx.resolveDidDocFromConnection(conn)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve did doc from exchange response connection: %w", err)
	}

	destination, err := service.CreateDestination(responseDidDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare destination from response did doc: %w", err)
	}

	docResolution, err := ctx.vdRegistry.Resolve(connRecord.MyDID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching did document: %w", err)
	}

	recKey, err := recipientKey(docResolution.DIDDocument)
	if err != nil {
		return nil, nil, fmt.Errorf("handle inbound response: %w", err)
	}

	return func() error {
		return ctx.outboundDispatcher.Send(ack, recKey, destination)
	}, connRecord, nil
}

// verifySignature verifies connection signature and returns connection.
func verifySignature(connSignature *ConnectionSignature, recipientKeys string) (*Connection, error) {
	sigData, err := base64.URLEncoding.DecodeString(connSignature.SignedData)
	if err != nil {
		return nil, fmt.Errorf("decode signature data: %w", err)
	}

	if len(sigData) == 0 {
		return nil, fmt.Errorf("missing or invalid signature data")
	}

	signature, err := base64.URLEncoding.DecodeString(connSignature.Signature)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	// The signature data must be used to verify against the invitation's recipientKeys for continuity.
	pubKey, err := fingerprint.PubKeyFromDIDKey(recipientKeys)
	if err != nil {
		return nil, fmt.Errorf(
			"verifySignature: failed to parse pubKeyBytes from recipientKeys [%s]: %w",
			recipientKeys, err,
		)
	}

	// TODO: Replace with signed attachments issue-626
	suiteVerifier := ed25519signature2018.NewPublicKeyVerifier()
	signatureSuite := ed25519signature2018.New(suite.WithVerifier(suiteVerifier))

	err = signatureSuite.Verify(&verifier.PublicKey{
		Type:  kms.ED25519,
		Value: pubKey,
	},
		sigData, signature)
	if err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}

	// trimming the timestamp and delimiter - only taking out connection attribute bytes
	if len(sigData) <= timestamplen {
		return nil, fmt.Errorf("missing connection attribute bytes")
	}

	connBytes := sigData[timestamplen:]
	conn := &Connection{}

	err = json.Unmarshal(connBytes, conn)
	if err != nil {
		return nil, fmt.Errorf("JSON unmarshalling of connection: %w", err)
	}

	return conn, nil
}

func getEpochTime() int64 {
	return time.Now().Unix()
}

func (ctx *context) getVerKey(invitationID string) (string, error) {
	pubKey, err := ctx.getVerKeyFromOOBInvitation(invitationID)
	if err != nil && !errors.Is(err, errVerKeyNotFound) {
		return "", fmt.Errorf("failed to get my verkey from oob invitation: %w", err)
	}

	if err == nil {
		return pubKey, nil
	}

	var invitation Invitation
	if isDID(invitationID) {
		invitation = Invitation{ID: invitationID, DID: invitationID}
	} else {
		err = ctx.connectionRecorder.GetInvitation(invitationID, &invitation)
		if err != nil {
			return "", fmt.Errorf("get invitation for signature [invitationID=%s]: %w", invitationID, err)
		}
	}

	invPubKey, err := ctx.getInvitationRecipientKey(&invitation)
	if err != nil {
		return "", fmt.Errorf("get invitation recipient key: %w", err)
	}

	return invPubKey, nil
}

func (ctx *context) getInvitationRecipientKey(invitation *Invitation) (string, error) {
	if invitation.DID != "" {
		docResolution, err := ctx.vdRegistry.Resolve(invitation.DID)
		if err != nil {
			return "", fmt.Errorf("get invitation recipient key: %w", err)
		}

		recKey, err := recipientKey(docResolution.DIDDocument)
		if err != nil {
			return "", fmt.Errorf("getInvitationRecipientKey: %w", err)
		}

		return recKey, nil
	}

	return invitation.RecipientKeys[0], nil
}

func (ctx *context) getVerKeyFromOOBInvitation(invitationID string) (string, error) {
	logger.Debugf("invitationID=%s", invitationID)

	var invitation OOBInvitation

	err := ctx.connectionRecorder.GetInvitation(invitationID, &invitation)
	if errors.Is(err, storage.ErrDataNotFound) {
		return "", errVerKeyNotFound
	}

	if err != nil {
		return "", fmt.Errorf("failed to load oob invitation: %w", err)
	}

	if invitation.Type != oobMsgType {
		return "", errVerKeyNotFound
	}

	pubKey, err := ctx.resolveVerKey(&invitation)
	if err != nil {
		return "", fmt.Errorf("failed to get my verkey: %w", err)
	}

	return pubKey, nil
}

func (ctx *context) getServiceBlock(i *OOBInvitation) (*did.Service, error) {
	logger.Debugf("extracting service block from oobinvitation=%+v", i)

	var block *did.Service

	switch svc := i.Target.(type) {
	case string:
		docResolution, err := ctx.vdRegistry.Resolve(svc)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve myDID=%s : %w", svc, err)
		}

		s, found := did.LookupService(docResolution.DIDDocument, didCommServiceType)
		if !found {
			return nil, fmt.Errorf(
				"no valid service block found on myDID=%s with serviceType=%s",
				svc, didCommServiceType)
		}

		block = s
	case *did.Service:
		block = svc
	case map[string]interface{}:
		var s did.Service

		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{TagName: "json", Result: &s})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize decoder : %w", err)
		}

		err = decoder.Decode(svc)
		if err != nil {
			return nil, fmt.Errorf("failed to decode service block : %w", err)
		}

		block = &s
	default:
		return nil, fmt.Errorf("unsupported target type: %+v", svc)
	}

	if len(i.MediaTypeProfiles) > 0 {
		// RFC0587: In case the accept property is set in both the DID service block and the out-of-band message,
		// the out-of-band property takes precedence.
		block.Accept = i.MediaTypeProfiles
	}

	logger.Debugf("extracted service block=%+v", block)

	return block, nil
}

func (ctx *context) resolveVerKey(i *OOBInvitation) (string, error) {
	logger.Debugf("extracting verkey from oobinvitation=%+v", i)

	svc, err := ctx.getServiceBlock(i)
	if err != nil {
		return "", fmt.Errorf("failed to get service block from oobinvitation : %w", err)
	}

	logger.Debugf("extracted verkey=%s", svc.RecipientKeys[0])

	return svc.RecipientKeys[0], nil
}

func isDID(str string) bool {
	const didPrefix = "did:"
	return strings.HasPrefix(str, didPrefix)
}

// returns the did:key ID of the first element in the doc's destination RecipientKeys.
func recipientKey(doc *did.Doc) (string, error) {
	dest, err := service.CreateDestination(doc)
	if err != nil {
		return "", fmt.Errorf("failed to create destination: %w", err)
	}

	return dest.RecipientKeys[0], nil
}
