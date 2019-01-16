/*
 * Copyright 2017 Kopano and its licensors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, version 3,
 * as published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/dgrijalva/jwt-go"
	"stash.kopano.io/kgol/rndm"

	"stash.kopano.io/kc/konnect"
	"stash.kopano.io/kc/konnect/identity"
	"stash.kopano.io/kc/konnect/oidc"
	"stash.kopano.io/kc/konnect/oidc/payload"
	"stash.kopano.io/kc/konnect/utils"
)

// MakeAccessToken implements the oidc.AccessTokenProvider interface.
func (p *Provider) MakeAccessToken(ctx context.Context, audience string, auth identity.AuthRecord) (string, error) {
	return p.makeAccessToken(ctx, audience, auth)
}

func (p *Provider) makeAccessToken(ctx context.Context, audience string, auth identity.AuthRecord) (string, error) {
	authorizedScopes := auth.AuthorizedScopes()
	authorizedScopesList := makeArrayFromBoolMap(authorizedScopes)

	accessTokenClaims := konnect.AccessTokenClaims{
		IsAccessToken:           true,
		AuthorizedScopesList:    authorizedScopesList,
		AuthorizedClaimsRequest: auth.AuthorizedClaims(),
		StandardClaims: jwt.StandardClaims{
			Issuer:    p.issuerIdentifier,
			Subject:   auth.Subject(),
			Audience:  audience,
			ExpiresAt: time.Now().Add(p.accessTokenDuration).Unix(),
			IssuedAt:  time.Now().Unix(),
			Id:        rndm.GenerateRandomString(24),
		},
	}

	user := auth.User()
	if user != nil {
		if userWithClaims, ok := user.(identity.UserWithClaims); ok {
			accessTokenClaims.IdentityClaims = userWithClaims.Claims()
		}
		accessTokenClaims.IdentityProvider = auth.Manager().Name()
	}

	accessToken := jwt.NewWithClaims(p.signingMethod, accessTokenClaims)
	accessToken.Header[oidc.JWTHeaderKeyID] = p.signingKeyID

	return accessToken.SignedString(p.signingKey)
}

func (p *Provider) makeIDToken(ctx context.Context, ar *payload.AuthenticationRequest, auth identity.AuthRecord, session *payload.Session, accessTokenString string, codeString string) (string, error) {
	publicSubject, err := p.PublicSubjectFromAuth(auth)
	if err != nil {
		return "", err
	}

	idTokenClaims := &oidc.IDTokenClaims{
		Nonce: ar.Nonce,
		StandardClaims: jwt.StandardClaims{
			Issuer:    p.issuerIdentifier,
			Subject:   publicSubject,
			Audience:  ar.ClientID,
			ExpiresAt: time.Now().Add(p.idTokenDuration).Unix(),
			IssuedAt:  time.Now().Unix(),
		},
	}

	if session != nil {
		// Include session data in ID token.
		idTokenClaims.SessionClaims = &oidc.SessionClaims{
			SessionID: session.ID,
		}
	}

	// Include requested scope data in ID token when no access token is
	// generated.
	authorizedClaimsRequest := auth.AuthorizedClaims()

	withAccessToken := accessTokenString != ""
	withCode := codeString != ""
	withAuthTime := ar.MaxAge > 0
	withIDTokenClaimsRequest := authorizedClaimsRequest != nil && authorizedClaimsRequest.IDToken != nil

	if withIDTokenClaimsRequest {
		// Apply additional information from ID token claims request.
		if _, ok := authorizedClaimsRequest.IDToken.Get(oidc.AuthTimeClaim); !withAuthTime && ok {
			// Return auth time claim if requested and not already requested by other means.
			withAuthTime = true
		}
	}

	if !withAccessToken || withIDTokenClaimsRequest {
		user := auth.User()
		if user == nil {
			return "", fmt.Errorf("no user")
		}

		var userID string
		if userWithClaims, ok := user.(identity.UserWithClaims); ok {
			identityClaims := userWithClaims.Claims()
			if userIDString, ok := identityClaims[konnect.IdentifiedUserIDClaim]; ok {
				userID = userIDString.(string)
			}
		}
		if userID == "" {
			return "", fmt.Errorf("no id claim in user identity claims")
		}

		var sessionRef *string
		if userWithSessionRef, ok := user.(identity.UserWithSessionRef); ok {
			sessionRef = userWithSessionRef.SessionRef()
		}

		var requestedClaimsMap []*payload.ClaimsRequestMap
		var requestedScopesMap map[string]bool
		if withIDTokenClaimsRequest {
			requestedClaimsMap = []*payload.ClaimsRequestMap{authorizedClaimsRequest.IDToken}
			requestedScopesMap = authorizedClaimsRequest.IDToken.ScopesMap(nil)
		}

		freshAuth, found, fetchErr := auth.Manager().Fetch(ctx, userID, sessionRef, auth.AuthorizedScopes(), requestedClaimsMap)
		if fetchErr != nil {
			p.logger.WithFields(utils.ErrorAsFields(fetchErr)).Errorln("identity manager fetch failed")
			found = false
		}
		if !found {
			return "", fmt.Errorf("user not found")
		}

		if (!withAccessToken && ar.Scopes[oidc.ScopeProfile]) || requestedScopesMap[oidc.ScopeProfile] {
			idTokenClaims.ProfileClaims = oidc.NewProfileClaims(freshAuth.Claims(oidc.ScopeProfile)[0])
		}
		if (!withAccessToken && ar.Scopes[oidc.ScopeEmail]) || requestedScopesMap[oidc.ScopeEmail] {
			idTokenClaims.EmailClaims = oidc.NewEmailClaims(freshAuth.Claims(oidc.ScopeEmail)[0])
		}

		auth = freshAuth
	}
	if withAccessToken {
		// Add left-most hash of access token.
		// http://openid.net/specs/openid-connect-core-1_0.html#ImplicitIDToken
		hash, hashErr := oidc.HashFromSigningMethod(p.signingMethod.Alg())
		if hashErr != nil {
			return "", hashErr
		}

		idTokenClaims.AccessTokenHash = oidc.LeftmostHash([]byte(accessTokenString), hash).String()
	}
	if withCode {
		// Add left-most hash of code.
		// http://openid.net/specs/openid-connect-core-1_0.html#HybridIDToken
		hash, hashErr := oidc.HashFromSigningMethod(p.signingMethod.Alg())
		if hashErr != nil {
			return "", hashErr
		}

		idTokenClaims.CodeHash = oidc.LeftmostHash([]byte(codeString), hash).String()
	}
	if withAuthTime {
		// Add AuthTime.
		if loggedOn, logonAt := auth.LoggedOn(); loggedOn {
			idTokenClaims.AuthTime = logonAt.Unix()
		} else {
			// NOTE(longsleep): Return current time to be spec compliant.
			idTokenClaims.AuthTime = time.Now().Unix()
		}
	}

	// Support extra non-standard claims in ID token.
	var finalIDTokenClaims jwt.Claims = idTokenClaims
	if !withAccessToken {
		// Include requested scope data in ID token when no access token is
		// generated - additional custom user specific claims.
		idTokenClaimsMap, err := payload.ToMap(idTokenClaims)
		if err != nil {
			return "", err
		}

		// Inject extra claims.
		extraClaims := auth.Claims("")[0]
		if extraClaims != nil {
			if extraClaimsMap, ok := extraClaims.(jwt.MapClaims); ok {
				for claim, value := range extraClaimsMap {
					idTokenClaimsMap[claim] = value
				}
			}
		}

		finalIDTokenClaims = jwt.MapClaims(idTokenClaimsMap)
	}

	// Create signed token.
	idToken := jwt.NewWithClaims(p.signingMethod, finalIDTokenClaims)
	idToken.Header[oidc.JWTHeaderKeyID] = p.signingKeyID

	return idToken.SignedString(p.signingKey)
}

func (p *Provider) makeRefreshToken(ctx context.Context, audience string, auth identity.AuthRecord) (string, error) {
	approvedScopesList := []string{}
	approvedScopes := make(map[string]bool)
	for scope, granted := range auth.AuthorizedScopes() {
		if granted {
			approvedScopesList = append(approvedScopesList, scope)
			approvedScopes[scope] = true
		}
	}

	ref, err := auth.Manager().ApproveScopes(ctx, auth.Subject(), audience, approvedScopes)
	if err != nil {
		return "", err
	}

	refreshTokenClaims := &konnect.RefreshTokenClaims{
		IsRefreshToken:        true,
		ApprovedScopesList:    approvedScopesList,
		ApprovedClaimsRequest: auth.AuthorizedClaims(),
		Ref:                   ref,
		StandardClaims: jwt.StandardClaims{
			Issuer:    p.issuerIdentifier,
			Subject:   auth.Subject(),
			Audience:  audience,
			ExpiresAt: time.Now().Add(p.refreshTokenDuration).Unix(),
			IssuedAt:  time.Now().Unix(),
			Id:        rndm.GenerateRandomString(24),
		},
	}

	user := auth.User()
	if user != nil {
		if userWithClaims, ok := user.(identity.UserWithClaims); ok {
			refreshTokenClaims.IdentityClaims = userWithClaims.Claims()
		}
		refreshTokenClaims.IdentityProvider = auth.Manager().Name()
	}

	refreshToken := jwt.NewWithClaims(p.signingMethod, refreshTokenClaims)
	refreshToken.Header[oidc.JWTHeaderKeyID] = p.signingKeyID

	return refreshToken.SignedString(p.signingKey)
}

func (p *Provider) validateJWT(token *jwt.Token) (interface{}, error) {
	rawAlg, ok := token.Header[oidc.JWTHeaderAlg]
	if !ok {
		return nil, fmt.Errorf("No alg header")
	}
	alg, ok := rawAlg.(string)
	if !ok {
		return nil, fmt.Errorf("Invalid alg value")
	}
	switch jwt.GetSigningMethod(alg).(type) {
	case *jwt.SigningMethodRSA:
	case *jwt.SigningMethodECDSA:
	case *jwt.SigningMethodRSAPSS:
	default:
		return nil, fmt.Errorf("Unexpected alg value")
	}
	rawKid, ok := token.Header[oidc.JWTHeaderKeyID]
	if !ok {
		return nil, fmt.Errorf("No kid header")
	}
	kid, ok := rawKid.(string)
	if !ok {
		return nil, fmt.Errorf("Invalid kid value")
	}
	key, ok := p.validationKeys[kid]
	if !ok {
		return nil, fmt.Errorf("Unknown kid")
	}
	return key, nil
}
