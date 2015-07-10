// Copyright 2015 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package publisher

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
)

// CTConfig defines the JSON configuration file schema
type CTConfig struct {
	Logs              []logDesc `json:"logs"`
	SubmissionRetries int       `json:"submissionRetries"`
	// This should use the same method as the DNS resolver
	SubmissionBackoffString string `json:"submissionBackoff"`

	SubmissionBackoff time.Duration `json:"-"`
	IssuerDER         []byte        `json:"-"`
}

type logDesc struct {
	URI string `json:"uri"`
}

type ctSubmissionRequest struct {
	Chain []string `json:"chain"`
}

type rawSignedCertificateTimestamp struct {
	Version    uint8  `json:"sct_version"`
	LogID      string `json:"id"`
	Timestamp  uint64 `json:"timestamp"`
	Signature  string `json:"signature"`
	Extensions string `json:"extensions"`
}

type signedCertificateTimestamp struct {
	SCTVersion uint8  // The version of the protocol to which the SCT conforms
	LogID      []byte // the SHA-256 hash of the log's public key, calculated over
	// the DER encoding of the key represented as SubjectPublicKeyInfo.
	Timestamp  uint64 // Timestamp (in ms since unix epoc) at which the SCT was issued
	Extensions []byte // For future extensions to the protocol
	Signature  []byte // The Log's signature for this SCT
}

func (sct *signedCertificateTimestamp) UnmarshalJSON(data []byte) error {
	var rawSCT rawSignedCertificateTimestamp
	var err error
	if err = json.Unmarshal(data, &rawSCT); err != nil {
		return fmt.Errorf("Failed to unmarshal SCT reciept, %s", err)
	}
	sct.LogID, err = base64.StdEncoding.DecodeString(rawSCT.LogID)
	if err != nil {
		return fmt.Errorf("Failed to decode log ID, %s", err)
	}
	sct.Signature, err = base64.StdEncoding.DecodeString(rawSCT.Signature)
	if err != nil {
		return fmt.Errorf("Failed to decode SCT signature, %s", err)
	}
	sct.Extensions, err = base64.StdEncoding.DecodeString(rawSCT.Extensions)
	if err != nil {
		return fmt.Errorf("Failed to decode SCT extensions, %s", err)
	}

	sct.SCTVersion = rawSCT.Version
	sct.Timestamp = rawSCT.Timestamp
	return nil
}

const (
	sctVersion       = 0
	sctSigType       = 0
	sctX509EntryType = 0
	sctHashSHA256    = 4
	sctSigECDSA      = 3
)

// PublisherAuthorityImpl defines a Publisher
type PublisherAuthorityImpl struct {
	log *blog.AuditLogger
	CT  *CTConfig
}

// NewPublisherAuthorityImpl creates a Publisher that will submit certificates
// to any CT logs configured in CTConfig
func NewPublisherAuthorityImpl(ctConfig *CTConfig, issuerDER []byte) (*PublisherAuthorityImpl, error) {
	var pub PublisherAuthorityImpl

	logger := blog.GetAuditLogger()
	logger.Notice("Publisher Authority Starting")
	pub.log = logger

	if ctConfig != nil {
		pub.CT = ctConfig
		pub.CT.IssuerDER = issuerDER
		ctBackoff, err := time.ParseDuration(ctConfig.SubmissionBackoffString)
		if err != nil {
			return nil, err
		}
		pub.CT.SubmissionBackoff = ctBackoff
	}

	return &pub, nil
}

// SubmitToCT will submit the certificate represented by certDER to any CT
// logs configured in pub.CT.Logs
func (pub PublisherAuthorityImpl) SubmitToCT(cert *x509.Certificate) error {
	if pub.CT == nil {
		return nil
	}
	submission := ctSubmissionRequest{Chain: []string{base64.StdEncoding.EncodeToString(cert.Raw), base64.StdEncoding.EncodeToString(pub.CT.IssuerDER)}}
	client := http.Client{}
	jsonSubmission, err := json.Marshal(submission)
	if err != nil {
		pub.log.Err(fmt.Sprintf("Unable to marshal CT submission, %s", err))
		return err
	}

	for _, ctLog := range pub.CT.Logs {
		done := false
		var retries int
		var sct signedCertificateTimestamp
		for !done && retries <= pub.CT.SubmissionRetries {
			resp, err := postJSON(&client, ctLog.URI, jsonSubmission, &sct)
			if err != nil {
				// Retry the request, log the error
				// AUDIT[ Error Conditions ] 9cc4d537-8534-4970-8665-4b382abe82f3
				pub.log.AuditErr(fmt.Errorf("Error POSTing JSON to CT log submission endpoint [%s]: %s", ctLog.URI, err))
				if retries >= pub.CT.SubmissionRetries {
					break
				}
				retries++
				time.Sleep(pub.CT.SubmissionBackoff)
				continue
			} else {
				if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusServiceUnavailable {
					// Retry the request after either 10 seconds or the period specified
					// by the Retry-After header
					backoff := pub.CT.SubmissionBackoff
					if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
						if seconds, err := strconv.Atoi(retryAfter); err != nil {
							backoff = time.Second * time.Duration(seconds)
						}
					}
					if retries >= pub.CT.SubmissionRetries {
						break
					}
					retries++
					time.Sleep(backoff)
					continue
				} else if resp.StatusCode != http.StatusOK {
					// Not something we expect to happen, set error, break loop and log
					// the error
					// AUDIT[ Error Conditions ] 9cc4d537-8534-4970-8665-4b382abe82f3
					pub.log.AuditErr(fmt.Errorf("Unexpected status code returned from CT log submission endpoint [%s]: Unexpected status code [%d]", ctLog.URI, resp.StatusCode))
					break
				}
			}

			done = true
			break
		}
		if !done {
			pub.log.Warning(fmt.Sprintf("Unable to submit certificate to CT log [Serial: %s, Log URI: %s, Retries: %d]", core.SerialToString(cert.SerialNumber), ctLog.URI, retries))
			return fmt.Errorf("Unable to submit certificate")
		}

		if err = sct.CheckSignature(); err != nil {
			// AUDIT[ Error Conditions ] 9cc4d537-8534-4970-8665-4b382abe82f3
			pub.log.AuditErr(err)
			return err
		}

		// Do something with the signedCertificateTimestamp, we might want to
		// include something in the CertificateStatus table or such to indicate
		// that it has been successfully submitted to CT logs so that we can retry
		// sometime in the future if it didn't work this time. (In the future this
		// will be needed anyway for putting SCT in OCSP responses)
		pub.log.Notice(fmt.Sprintf("Submitted certificate to CT log [Serial: %s, Log URI: %s, Retries: %d]", core.SerialToString(cert.SerialNumber), ctLog.URI, retries))
	}

	return nil
}

func postJSON(client *http.Client, uri string, data []byte, respObj interface{}) (*http.Response, error) {
	req, err := http.NewRequest("POST", uri, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("Creating request failed, %s", err)
	}
	req.Header.Set("Keep-Alive", "timeout=15, max=100")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Request failed, %s", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to read response body, %s", err)
	}

	err = json.Unmarshal(body, respObj)
	if err != nil {
		return nil, fmt.Errorf("Failed to unmarshal SCT reciept, %s", err)
	}

	return resp, nil
}

// CheckSignature validates that the returned SCT signature is a valid SHA256 +
// ECDSA signature but does not verify that a specific public key signed it.
func (sct *signedCertificateTimestamp) CheckSignature() error {
	if len(sct.Signature) < 4 {
		return errors.New("SCT signature is truncated")
	}
	// Since all of the known logs currently only use SHA256 hashes and ECDSA
	// keys, only allow those
	if sct.Signature[0] != sctHashSHA256 {
		return fmt.Errorf("Unsupported SCT hash function [%d]", sct.Signature[0])
	}
	if sct.Signature[1] != sctSigECDSA {
		return fmt.Errorf("Unsupported SCT signature algorithm [%d]", sct.Signature[1])
	}

	var ecdsaSig struct {
		R, S *big.Int
	}
	// Ignore the two length bytes and attempt to unmarshal the signature directly
	signatureBytes := sct.Signature[4:]
	signatureBytes, err := asn1.Unmarshal(signatureBytes, &ecdsaSig)
	if err != nil {
		return fmt.Errorf("Failed to parse SCT signature, %s", err)
	}
	if len(signatureBytes) > 0 {
		return fmt.Errorf("Trailing garbage after signature")
	}

	return nil
}
