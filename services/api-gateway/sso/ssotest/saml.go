// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package ssotest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"math/big"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

// SAMLIdP signs SAML responses with a real key and certificate.
type SAMLIdP struct {
	EntityID string
	SSOURL   string
	SLOURL   string
	Key      *rsa.PrivateKey
	Cert     *x509.Certificate
}

// NewSAMLIdP creates an IdP with a fresh self-signed signing certificate.
func NewSAMLIdP(entityID string) *SAMLIdP {
	key, cert := selfSigned()
	return &SAMLIdP{EntityID: entityID, SSOURL: entityID + "/sso", SLOURL: entityID + "/slo", Key: key, Cert: cert}
}

func selfSigned() (*rsa.PrivateKey, *x509.Certificate) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "ssotest-idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return key, cert
}

// Metadata is the IdP's metadata XML.
func (i *SAMLIdP) Metadata() string {
	ed := saml.EntityDescriptor{
		EntityID: i.EntityID,
		IDPSSODescriptors: []saml.IDPSSODescriptor{{
			SSODescriptor: saml.SSODescriptor{
				RoleDescriptor: saml.RoleDescriptor{
					ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
					KeyDescriptors: []saml.KeyDescriptor{{
						Use: "signing",
						KeyInfo: saml.KeyInfo{X509Data: saml.X509Data{X509Certificates: []saml.X509Certificate{
							{Data: base64.StdEncoding.EncodeToString(i.Cert.Raw)},
						}}},
					}},
				},
				SingleLogoutServices: []saml.Endpoint{{Binding: saml.HTTPRedirectBinding, Location: i.SLOURL}},
			},
			SingleSignOnServices: []saml.Endpoint{{Binding: saml.HTTPRedirectBinding, Location: i.SSOURL}},
		}},
	}
	b, err := xml.Marshal(ed)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ResponseOpts shapes one Response; zero values give a valid response.
type ResponseOpts struct {
	ACS, SPEntityID, RequestID string
	NameID                     string
	NameIDFormat               string
	Email                      string
	EmailVerified              bool
	Groups                     []string
	AssertionID                string
	Now                        time.Time

	Audience       string // override; "" means SPEntityID
	NoAudience     bool
	Recipient      string // override; "" means ACS
	Destination    string // override; "" means ACS
	InResponseTo   string // override for the SubjectConfirmationData; "" means RequestID
	Issuer         string // override; "" means the IdP entity ID
	Lifetime       time.Duration
	NoSubjectConf  bool
	NoConditions   bool
	UnsignedAssert bool // do not sign the assertion
	SignResponse   bool // also sign the Response
	// SignWithOtherKey signs with an unrelated key (tampering).
	SignWithOtherKey bool
	// AfterSign mutates the signed document (tampering, wrapping).
	AfterSign func(doc *etree.Document)
}

const (
	nsP = "urn:oasis:names:tc:SAML:2.0:protocol"
	nsA = "urn:oasis:names:tc:SAML:2.0:assertion"
)

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Response builds and signs a SAMLResponse, returned base64 like the POST
// binding sends it.
func (i *SAMLIdP) Response(o ResponseOpts) string {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Lifetime == 0 {
		o.Lifetime = 5 * time.Minute
	}
	if o.AssertionID == "" {
		o.AssertionID = "_a" + RandomString()[:20]
	}
	if o.NameIDFormat == "" {
		o.NameIDFormat = string(saml.PersistentNameIDFormat)
	}
	pick := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	doc := etree.NewDocument()
	resp := doc.CreateElement("samlp:Response")
	resp.CreateAttr("xmlns:samlp", nsP)
	resp.CreateAttr("xmlns:saml", nsA)
	resp.CreateAttr("ID", "_r"+RandomString()[:20])
	resp.CreateAttr("Version", "2.0")
	resp.CreateAttr("IssueInstant", ts(o.Now))
	resp.CreateAttr("Destination", pick(o.Destination, o.ACS))
	resp.CreateAttr("InResponseTo", o.RequestID)
	resp.CreateElement("saml:Issuer").SetText(pick(o.Issuer, i.EntityID))
	resp.CreateElement("samlp:Status").CreateElement("samlp:StatusCode").CreateAttr("Value", saml.StatusSuccess)

	a := i.Assertion(o)
	key := i.Key
	if o.SignWithOtherKey {
		key, _ = selfSigned()
	}
	if !o.UnsignedAssert {
		a = sign(a, key, i.Cert)
	}
	resp.AddChild(a)
	if o.SignResponse {
		signed := sign(resp, key, i.Cert)
		doc.SetRoot(signed)
	}
	if o.AfterSign != nil {
		o.AfterSign(doc)
	}
	b, err := doc.WriteToBytes()
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Assertion builds an unsigned assertion element.
func (i *SAMLIdP) Assertion(o ResponseOpts) *etree.Element {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Lifetime == 0 {
		o.Lifetime = 5 * time.Minute
	}
	if o.NameIDFormat == "" {
		o.NameIDFormat = string(saml.PersistentNameIDFormat)
	}
	pick := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	a := etree.NewElement("saml:Assertion")
	a.CreateAttr("xmlns:saml", nsA)
	a.CreateAttr("ID", pick(o.AssertionID, "_a"+RandomString()[:20]))
	a.CreateAttr("Version", "2.0")
	a.CreateAttr("IssueInstant", ts(o.Now))
	a.CreateElement("saml:Issuer").SetText(pick(o.Issuer, i.EntityID))
	subj := a.CreateElement("saml:Subject")
	nid := subj.CreateElement("saml:NameID")
	nid.CreateAttr("Format", o.NameIDFormat)
	nid.SetText(o.NameID)
	if !o.NoSubjectConf {
		sc := subj.CreateElement("saml:SubjectConfirmation")
		sc.CreateAttr("Method", "urn:oasis:names:tc:SAML:2.0:cm:bearer")
		scd := sc.CreateElement("saml:SubjectConfirmationData")
		scd.CreateAttr("InResponseTo", pick(o.InResponseTo, o.RequestID))
		scd.CreateAttr("NotOnOrAfter", ts(o.Now.Add(o.Lifetime)))
		scd.CreateAttr("Recipient", pick(o.Recipient, o.ACS))
	}
	if !o.NoConditions {
		c := a.CreateElement("saml:Conditions")
		c.CreateAttr("NotBefore", ts(o.Now.Add(-time.Minute)))
		c.CreateAttr("NotOnOrAfter", ts(o.Now.Add(o.Lifetime)))
		if !o.NoAudience {
			c.CreateElement("saml:AudienceRestriction").CreateElement("saml:Audience").SetText(pick(o.Audience, o.SPEntityID))
		}
	}
	as := a.CreateElement("saml:AuthnStatement")
	as.CreateAttr("AuthnInstant", ts(o.Now))
	as.CreateAttr("SessionIndex", "_s"+RandomString()[:12])
	as.CreateElement("saml:AuthnContext").CreateElement("saml:AuthnContextClassRef").SetText("urn:oasis:names:tc:SAML:2.0:ac:classes:Password")
	st := a.CreateElement("saml:AttributeStatement")
	attr := func(name string, values ...string) {
		at := st.CreateElement("saml:Attribute")
		at.CreateAttr("Name", name)
		for _, v := range values {
			at.CreateElement("saml:AttributeValue").SetText(v)
		}
	}
	if o.Email != "" {
		attr("email", o.Email)
	}
	if o.EmailVerified {
		attr("email_verified", "true")
	}
	if len(o.Groups) > 0 {
		attr("groups", o.Groups...)
	}
	return a
}

func sign(el *etree.Element, key *rsa.PrivateKey, cert *x509.Certificate) *etree.Element {
	ks := dsig.TLSCertKeyStore(tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key})
	ctx := dsig.NewDefaultSigningContext(ks)
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if err := ctx.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
		panic(err)
	}
	signed, err := ctx.SignEnveloped(el)
	if err != nil {
		panic(err)
	}
	return signed
}
