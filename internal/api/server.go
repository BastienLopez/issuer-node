package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	core "github.com/iden3/go-iden3-core"
	"github.com/iden3/go-schema-processor/processor"

	"github.com/polygonid/sh-id-platform/internal/common"
	"github.com/polygonid/sh-id-platform/internal/config"
	"github.com/polygonid/sh-id-platform/internal/core/domain"
	"github.com/polygonid/sh-id-platform/internal/core/ports"
	"github.com/polygonid/sh-id-platform/internal/log"
	"github.com/polygonid/sh-id-platform/internal/repositories"
	"github.com/polygonid/sh-id-platform/pkg/rand"
)

// Server implements StrictServerInterface and holds the implementation of all API controllers
// This is the glue to the API autogenerated code
type Server struct {
	cfg             *config.Configuration
	identityService ports.IndentityService
	claimService    ports.ClaimsService
	schemaService   ports.SchemaService
}

// NewServer is a Server constructor
func NewServer(cfg *config.Configuration, identityService ports.IndentityService, claimsService ports.ClaimsService, schemaService ports.SchemaService) *Server {
	return &Server{
		cfg:             cfg,
		identityService: identityService,
		claimService:    claimsService,
		schemaService:   schemaService,
	}
}

// Health is a method
func (s *Server) Health(_ context.Context, _ HealthRequestObject) (HealthResponseObject, error) {
	return Health200JSONResponse{
		Cache: true,
		Db:    false,
	}, nil
}

// GetDocumentation this method will be overridden in the main function
func (s *Server) GetDocumentation(_ context.Context, _ GetDocumentationRequestObject) (GetDocumentationResponseObject, error) {
	return nil, nil
}

// GetYaml this method will be overridden in the main function
func (s *Server) GetYaml(_ context.Context, _ GetYamlRequestObject) (GetYamlResponseObject, error) {
	return nil, nil
}

// RegisterStatic add method to the mux that are not documented in the API.
func RegisterStatic(mux *chi.Mux) {
	mux.Get("/", documentation)
	mux.Get("/static/docs/api/api.yaml", swagger)
}

func documentation(w http.ResponseWriter, _ *http.Request) {
	writeFile("api/spec.html", w)
}

func swagger(w http.ResponseWriter, _ *http.Request) {
	writeFile("api/api.yaml", w)
}

func writeFile(path string, w http.ResponseWriter) {
	f, err := os.ReadFile(path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f)
}

// CreateIdentity is created identity controller
func (s *Server) CreateIdentity(ctx context.Context, request CreateIdentityRequestObject) (CreateIdentityResponseObject, error) {
	identity, err := s.identityService.Create(ctx, fmt.Sprintf("%s:%d", s.cfg.ServerUrl, s.cfg.ServerPort))
	if err != nil {
		return nil, err
	}
	return CreateIdentity201JSONResponse{
		Identifier: &identity.Identifier,
		Immutable:  identity.Immutable,
		Relay:      identity.Relay,
		State: &IdentityState{
			BlockNumber:        identity.State.BlockNumber,
			BlockTimestamp:     identity.State.BlockTimestamp,
			ClaimsTreeRoot:     identity.State.ClaimsTreeRoot,
			CreatedAt:          identity.State.CreatedAt,
			ModifiedAt:         identity.State.ModifiedAt,
			PreviousState:      identity.State.PreviousState,
			RevocationTreeRoot: identity.State.RevocationTreeRoot,
			RootOfRoots:        identity.State.RootOfRoots,
			State:              identity.State.State,
			Status:             string(identity.State.Status),
			TxID:               identity.State.TxID,
		},
	}, nil
}

// CreateClaim is claim creation controller
func (s *Server) CreateClaim(ctx context.Context, request CreateClaimRequestObject) (CreateClaimResponseObject, error) {
	if request.Identifier == "" {
		return CreateClaim400JSONResponse{N400JSONResponse{Message: "Invalid request identifier"}}, nil
	}

	did, err := core.ParseDID(request.Identifier)
	if err != nil {
		return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
	}

	schema, err := s.schemaService.LoadSchema(ctx, request.Body.CredentialSchema)
	if err != nil {
		return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
	}

	claimReq := ports.NewClaimRequest(schema, did, request.Body.CredentialSchema, request.Body.CredentialSubject, request.Body.Expiration, request.Body.Type, request.Body.Version, request.Body.SubjectPosition, request.Body.MerklizedRootPosition)

	nonce, err := rand.Int64()
	if err != nil {
		log.Error(ctx, "Can not create a nonce", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	vc, err := s.claimService.CreateVC(ctx, claimReq, nonce)
	if err != nil {
		log.Error(ctx, "Can not create a claim", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	jsonLdContext, ok := schema.Metadata.Uris["jsonLdContext"].(string)
	if !ok {
		log.Warn(ctx, "invalid jsonLdContext")
		return CreateClaim400JSONResponse{N400JSONResponse{Message: "invalid jsonLdContext"}}, nil
	}

	credentialType := fmt.Sprintf("%s#%s", jsonLdContext, request.Body.Type)
	mtRootPostion := common.DefineMerklizedRootPosition(schema.Metadata, claimReq.MerklizedRootPosition)

	coreClaim, err := s.schemaService.Process(ctx, claimReq.CredentialSchema, credentialType, vc, &processor.CoreClaimOptions{
		RevNonce:              nonce,
		MerklizedRootPosition: mtRootPostion,
		Version:               claimReq.Version,
		SubjectPosition:       claimReq.SubjectPos,
		Updatable:             false,
	})
	if err != nil {
		log.Error(ctx, "Can not process the schema", err)
		return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
	}

	claim, err := domain.FromClaimer(coreClaim, claimReq.CredentialSchema, credentialType)
	if err != nil {
		log.Error(ctx, "Can not obtain the claim from claimer", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	authClaim, err := s.claimService.GetAuthClaim(ctx, did)
	if err != nil {
		log.Error(ctx, "Can not retrieve the auth claim", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil

	}

	proof, err := s.identityService.SignClaimEntry(ctx, authClaim,
		coreClaim)
	if err != nil {
		log.Error(ctx, "Can not sign claim entry", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	issuerDIDString := did.String()
	claim.Identifier = &issuerDIDString
	claim.Issuer = issuerDIDString

	proof.IssuerData.CredentialStatus = s.claimService.GetRevocationSource(issuerDIDString, uint64(authClaim.RevNonce))

	jsonSignatureProof, err := json.Marshal(proof)
	if err != nil {
		log.Error(ctx, "Can not encode the json signature proof", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}
	err = claim.SignatureProof.Set(jsonSignatureProof)
	if err != nil {
		log.Error(ctx, "Can not set the json signature proof", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	err = claim.Data.Set(vc)
	if err != nil {
		log.Error(ctx, "Can not set the credential", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	err = claim.CredentialStatus.Set(vc.CredentialStatus)
	if err != nil {
		log.Error(ctx, "Can not set the credential status", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	claimResp, err := s.claimService.Save(ctx, claim)
	if err != nil {
		log.Error(ctx, "Can not save the claim", err)
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}

	return CreateClaim201JSONResponse{Id: claimResp.ID.String()}, nil
}

// RevokeClaim is the revocation claim controller
func (s *Server) RevokeClaim(ctx context.Context, request RevokeClaimRequestObject) (RevokeClaimResponseObject, error) {
	if err := s.claimService.Revoke(ctx, request.Identifier, uint64(request.Nonce), ""); err != nil {
		if errors.Is(err, repositories.ErrClaimDoesNotExist) {
			return RevokeClaim404JSONResponse{N404JSONResponse{
				Message: "the claim does not exist",
			}}, nil
		}

		return RevokeClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}
	return RevokeClaim202JSONResponse{
		Status: "pending",
	}, nil
}

// GetRevocationStatus is the controller to get revocation status
func (s *Server) GetRevocationStatus(ctx context.Context, request GetRevocationStatusRequestObject) (GetRevocationStatusResponseObject, error) {
	return nil, nil
}

// PublishState is the controller to publish the state on-chain
func (s *Server) PublishState(ctx context.Context, request PublishStateRequestObject) (PublishStateResponseObject, error) {
	return nil, nil
}

func (s *Server) GetClaim(ctx context.Context, request GetClaimRequestObject) (GetClaimResponseObject, error) {
	return nil, nil
}
