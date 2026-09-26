// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sso

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	xrv "github.com/mattermost/xml-roundtrip-validator"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// SAML errors, all mapped to one opaque code for the user.
var (
	ErrSAMLMalformed      = errors.New("saml: response is not well-formed XML")
	ErrSAMLAssertionCount = errors.New("saml: response must contain exactly one Assertion and no EncryptedAssertion")
	ErrSAMLUnsigned       = errors.New("saml: the Assertion is not signed")
	ErrSAMLNoSubject      = errors.New("saml: assertion has no Subject, NameID or bearer SubjectConfirmation")
	ErrSAMLNoConditions   = errors.New("saml: assertion has no Conditions")
	ErrSAMLAudience       = errors.New("saml: assertion AudienceRestriction does not name this service provider")
	ErrSAMLTransientName  = errors.New("saml: transient NameID cannot identify a returning user; configure a persistent NameID")
	ErrSAMLTooLarge       = errors.New("saml: response is too large")
)

const (
	nsAssertion = "urn:oasis:names:tc:SAML:2.0:assertion"
	nsProtocol  = "urn:oasis:names:tc:SAML:2.0:protocol"
	nsDSig      = "http://www.w3.org/2000/09/xmldsig#"
	bearer      = "urn:oasis:names:tc:SAML:2.0:cm:bearer"
	// MaxSAMLResponse bounds the base64 SAMLResponse accepted by the ACS.
	MaxSAMLResponse = 256 << 10
)

// SAML is the service provider. Stateless apart from its config.
type SAML struct{ cfg *Config }

// NewSAML builds the service provider.
func NewSAML(cfg *Config) *SAML { return &SAML{cfg: cfg} }

// ParseIdPMetadata parses pasted IdP metadata (an EntityDescriptor, or an
// EntitiesDescriptor holding one IdP) and checks it is usable: an
// IDPSSODescriptor with an HTTP-Redirect SSO endpoint and at least one signing
// certificate. Returns the IdP entity ID.
func ParseIdPMetadata(raw string) (*saml.EntityDescriptor, error) {
	if len(raw) > 512<<10 {
		return nil, errors.New("metadata is larger than 512 KiB")
	}
	if err := xrv.Validate(strings.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("metadata is not well-formed XML: %v", err)
	}
	var ed saml.EntityDescriptor
	if err := xml.Unmarshal([]byte(raw), &ed); err != nil || len(ed.IDPSSODescriptors) == 0 {
		var eds saml.EntitiesDescriptor
		if err2 := xml.Unmarshal([]byte(raw), &eds); err2 != nil {
			return nil, errors.New("metadata is neither an EntityDescriptor nor an EntitiesDescriptor")
		}
		found := false
		for _, e := range eds.EntityDescriptors {
			if len(e.IDPSSODescriptors) > 0 {
				if found {
					return nil, errors.New("metadata describes more than one identity provider; paste one")
				}
				ed, found = e, true
			}
		}
		if !found {
			return nil, errors.New("metadata has no IDPSSODescriptor")
		}
	}
	if ed.EntityID == "" {
		return nil, errors.New("metadata has no entityID")
	}
	idp := ed.IDPSSODescriptors[0]
	redirect := false
	for _, s := range idp.SingleSignOnServices {
		if s.Binding == saml.HTTPRedirectBinding && s.Location != "" {
			redirect = true
		}
	}
	if !redirect {
		return nil, errors.New("metadata has no HTTP-Redirect SingleSignOnService")
	}
	signing := false
	for _, kd := range idp.KeyDescriptors {
		if (kd.Use == "" || kd.Use == "signing") && len(kd.KeyInfo.X509Data.X509Certificates) > 0 {
			signing = true
		}
	}
	if !signing {
		return nil, errors.New("metadata has no signing certificate")
	}
	return &ed, nil
}

func (s *SAML) sp(p store.SSOProviderRow) (*saml.ServiceProvider, error) {
	md, err := ParseIdPMetadata(p.IDPMetadataXML)
	if err != nil {
		return nil, err
	}
	acs, _ := url.Parse(s.cfg.SAMLACSURL(p.ID))
	meta, _ := url.Parse(s.cfg.SAMLEntityID(p.ID))
	slo, _ := url.Parse(s.cfg.SAMLSLOURL(p.ID))
	sp := &saml.ServiceProvider{
		EntityID:    meta.String(),
		MetadataURL: *meta,
		AcsURL:      *acs,
		SloURL:      *slo,
		IDPMetadata: md,
		// Persistent: the NameID is the stable subject identities are keyed on.
		AuthnNameIDFormat: saml.PersistentNameIDFormat,
		AllowIDPInitiated: false,
		// crewjam accepts an assertion with NO AudienceRestriction; require one
		// that names this SP.
		ValidateAudienceRestriction: func(a *saml.Assertion) error {
			if a.Conditions == nil {
				return ErrSAMLNoConditions
			}
			for _, ar := range a.Conditions.AudienceRestrictions {
				if ar.Audience.Value == meta.String() {
					return nil
				}
			}
			return ErrSAMLAudience
		},
	}
	return sp, nil
}

// Metadata returns this provider's SP metadata document.
func (s *SAML) Metadata(p store.SSOProviderRow) ([]byte, error) {
	sp, err := s.sp(p)
	if err != nil {
		return nil, err
	}
	md := sp.Metadata()
	// Signed assertions are required (ADR-0024 §4); say so to the IdP.
	t := true
	for i := range md.SPSSODescriptors {
		md.SPSSODescriptors[i].WantAssertionsSigned = &t
	}
	return xml.MarshalIndent(md, "", "  ")
}

// AuthnRequestURL builds an SP-initiated AuthnRequest (HTTP-Redirect binding)
// and returns the URL and the request ID to bind InResponseTo to.
func (s *SAML) AuthnRequestURL(p store.SSOProviderRow, relayState string) (string, string, error) {
	sp, err := s.sp(p)
	if err != nil {
		return "", "", err
	}
	loc := sp.GetSSOBindingLocation(saml.HTTPRedirectBinding)
	req, err := sp.MakeAuthenticationRequest(loc, saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return "", "", err
	}
	u, err := req.Redirect(relayState, sp)
	if err != nil {
		return "", "", err
	}
	return u.String(), req.ID, nil
}

// SAMLResult is a verified assertion's identity and replay metadata.
type SAMLResult struct {
	Identity     store.SSOAssertedIdentity
	AssertionID  string
	NotOnOrAfter time.Time
	NameID       string
	SessionIndex string
}

// ParseResponse verifies a base64 SAMLResponse posted to the ACS.
//
// Checks, in order (ADR-0024 §4):
//  1. size bound, base64, XML round-trip safety (crewjam's validator);
//  2. exactly one Assertion, no EncryptedAssertion, and the Assertion itself
//     carries a ds:Signature, a Subject and Conditions;
//  3. crewjam on the original document: signatures against the stored IdP
//     certificates only, Destination, Issuer, InResponseTo, Recipient,
//     NotBefore/NotOnOrAfter, audience;
//  4. when the Response is ALSO signed, crewjam again on a copy with the
//     Response signature removed, so the Assertion signature itself is what is
//     verified (crewjam otherwise trusts any assertion inside a signed
//     response);
//  5. a bearer SubjectConfirmation and a non-transient NameID.
func (s *SAML) ParseResponse(p store.SSOProviderRow, samlResponse, requestID string) (SAMLResult, error) {
	var res SAMLResult
	if len(samlResponse) > MaxSAMLResponse {
		return res, ErrSAMLTooLarge
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(samlResponse))
	if err != nil {
		return res, ErrSAMLMalformed
	}
	if err := xrv.Validate(bytes.NewReader(raw)); err != nil {
		return res, ErrSAMLMalformed
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(raw); err != nil || doc.Root() == nil {
		return res, ErrSAMLMalformed
	}
	root := doc.Root()
	if root.NamespaceURI() != nsProtocol || root.Tag != "Response" {
		return res, ErrSAMLMalformed
	}
	var assertions []*etree.Element
	for _, c := range root.ChildElements() {
		if c.NamespaceURI() == nsAssertion && c.Tag == "EncryptedAssertion" {
			return res, ErrSAMLAssertionCount
		}
		if c.NamespaceURI() == nsAssertion && c.Tag == "Assertion" {
			assertions = append(assertions, c)
		}
	}
	// Also refuse assertions hidden deeper (e.g. in Extensions): one assertion
	// in the whole document, and it is a direct child of the Response.
	if len(assertions) != 1 || countAssertions(root) != 1 {
		return res, ErrSAMLAssertionCount
	}
	a := assertions[0]
	if !hasChild(a, nsDSig, "Signature") {
		return res, ErrSAMLUnsigned
	}
	if !hasChild(a, nsAssertion, "Subject") {
		return res, ErrSAMLNoSubject
	}
	if !hasChild(a, nsAssertion, "Conditions") {
		return res, ErrSAMLNoConditions
	}

	sp, err := s.sp(p)
	if err != nil {
		return res, err
	}
	acs := sp.AcsURL
	assertion, err := sp.ParseXMLResponse(raw, []string{requestID}, acs)
	if err != nil {
		return res, fmt.Errorf("saml: %w", privateErr(err))
	}
	if sig := childElement(root, nsDSig, "Signature"); sig != nil {
		root.RemoveChild(sig)
		stripped, werr := doc.WriteToBytes()
		if werr != nil {
			return res, ErrSAMLMalformed
		}
		if _, err := sp.ParseXMLResponse(stripped, []string{requestID}, acs); err != nil {
			return res, fmt.Errorf("saml: assertion signature: %w", privateErr(err))
		}
	}

	if assertion.Subject == nil || assertion.Subject.NameID == nil || strings.TrimSpace(assertion.Subject.NameID.Value) == "" {
		return res, ErrSAMLNoSubject
	}
	if assertion.Subject.NameID.Format == string(saml.TransientNameIDFormat) {
		return res, ErrSAMLTransientName
	}
	hasBearer := false
	for _, sc := range assertion.Subject.SubjectConfirmations {
		if sc.Method == bearer && sc.SubjectConfirmationData != nil {
			hasBearer = true
			if res.NotOnOrAfter.IsZero() || sc.SubjectConfirmationData.NotOnOrAfter.Before(res.NotOnOrAfter) {
				res.NotOnOrAfter = sc.SubjectConfirmationData.NotOnOrAfter
			}
		}
	}
	if !hasBearer {
		return res, ErrSAMLNoSubject
	}
	if assertion.Conditions != nil && !assertion.Conditions.NotOnOrAfter.IsZero() &&
		(res.NotOnOrAfter.IsZero() || assertion.Conditions.NotOnOrAfter.After(res.NotOnOrAfter)) {
		res.NotOnOrAfter = assertion.Conditions.NotOnOrAfter
	}
	if res.NotOnOrAfter.IsZero() {
		res.NotOnOrAfter = s.cfg.now().Add(10 * time.Minute)
	}
	res.NotOnOrAfter = res.NotOnOrAfter.Add(saml.MaxClockSkew)
	res.AssertionID = assertion.ID
	if res.AssertionID == "" {
		return res, ErrSAMLMalformed
	}
	res.NameID = assertion.Subject.NameID.Value
	for _, st := range assertion.AuthnStatements {
		if st.SessionIndex != "" {
			res.SessionIndex = st.SessionIndex
		}
	}
	res.Identity = identityFromAttributes(assertion, p.GroupsClaim)
	res.Identity.Subject = res.NameID
	return res, nil
}

// privateErr unwraps crewjam's InvalidResponseError, whose Error() is a
// static string, so the audit log records the real reason.
func privateErr(err error) error {
	var ire *saml.InvalidResponseError
	if errors.As(err, &ire) && ire.PrivateErr != nil {
		return ire.PrivateErr
	}
	return err
}

func countAssertions(el *etree.Element) int {
	n := 0
	for _, c := range el.ChildElements() {
		if c.Tag == "Assertion" || c.Tag == "EncryptedAssertion" {
			n++
		}
		n += countAssertions(c)
	}
	return n
}

func childElement(el *etree.Element, ns, tag string) *etree.Element {
	for _, c := range el.ChildElements() {
		if c.Tag == tag && c.NamespaceURI() == ns {
			return c
		}
	}
	return nil
}

func hasChild(el *etree.Element, ns, tag string) bool { return childElement(el, ns, tag) != nil }

var (
	emailAttrs = []string{"email", "mail", "emailaddress", "urn:oid:0.9.2342.19200300.100.1.3",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"}
	nameAttrs = []string{"name", "displayname", "urn:oid:2.16.840.1.113730.3.1.241",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name"}
)

func identityFromAttributes(a *saml.Assertion, groupsAttr string) store.SSOAssertedIdentity {
	var id store.SSOAssertedIdentity
	values := map[string][]string{}
	for _, st := range a.AttributeStatements {
		for _, at := range st.Attributes {
			for _, v := range at.Values {
				val := strings.TrimSpace(v.Value)
				if val == "" {
					continue
				}
				for _, k := range []string{at.Name, at.FriendlyName} {
					if k != "" {
						values[strings.ToLower(k)] = append(values[strings.ToLower(k)], val)
					}
				}
			}
		}
	}
	first := func(names []string) string {
		for _, n := range names {
			if v := values[strings.ToLower(n)]; len(v) > 0 {
				return v[0]
			}
		}
		return ""
	}
	id.Email = strings.ToLower(first(emailAttrs))
	id.DisplayName = first(nameAttrs)
	if id.DisplayName == "" {
		id.DisplayName = strings.TrimSpace(first([]string{"givenname", "firstname"}) + " " + first([]string{"surname", "sn", "lastname"}))
	}
	// SAML has no standard email_verified; honour an explicit attribute of that
	// name and nothing else. Absent means NOT verified, so SAML never
	// auto-links by email unless the IdP is configured to assert it.
	id.EmailVerified = strings.EqualFold(first([]string{"email_verified"}), "true")
	if groupsAttr == "" {
		groupsAttr = "groups"
	}
	id.Groups = capGroups(values[strings.ToLower(groupsAttr)])
	return id
}

// LogoutURL returns an SP-initiated LogoutRequest URL (HTTP-Redirect), or ""
// when the IdP metadata has no SLO endpoint.
func (s *SAML) LogoutURL(p store.SSOProviderRow, nameID, relayState string) (string, error) {
	sp, err := s.sp(p)
	if err != nil {
		return "", err
	}
	if sp.GetSLOBindingLocation(saml.HTTPRedirectBinding) == "" || nameID == "" {
		return "", nil
	}
	u, err := sp.MakeRedirectLogoutRequest(nameID, relayState)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
