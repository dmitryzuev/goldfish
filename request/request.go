package request

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/caiyeon/goldfish/vault"
	"github.com/gorilla/securecookie"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/helper/xor"
	"github.com/mitchellh/hashstructure"
	"github.com/mitchellh/mapstructure"
)

type Request interface {
	IsRootOnly()                   bool
	Verify(vault.AuthInfo)         error
	Approve(string, string)        error
	Reject(vault.AuthInfo, string) error
	Create(vault.AuthInfo, map[string]interface{}) (string, error)
}

// adds a request if user has authentication
func Add(auth vault.AuthInfo, raw map[string]interface{}) (string, error) {
	t := ""
	if typeRaw, ok := raw["Type"]; ok {
		t, ok = typeRaw.(string)
	}
	if t == "" {
		return "", errors.New("Type field is empty")
	}

	switch strings.ToLower(t) {
	case "policy":
		var req PolicyRequest
		return req.Create(auth, raw)

	default:
		return "", errors.New("Unsupported request type")
	}
}

// fetches a request if it exists, and if user has authentication
func Get(auth vault.AuthInfo, hash string) (Request, error) {
	// fetch request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("requests/" + hash)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("Change ID not found")
	}

	// decode secret to a request
	t := ""
	if raw, ok := resp.Data["Type"]; ok {
		t, ok = raw.(string)
	}
	if t == "" {
		return nil, errors.New("Invalid request type")
	}

	switch strings.ToLower(t) {
	case "policy":
		// decode secret into policy request
		var req PolicyRequest
		if err := mapstructure.Decode(resp.Data, &req); err != nil {
			return nil, err
		}
		// verify hash
		hash_uint64, err := hashstructure.Hash(req, nil)
		if err != nil || strconv.FormatUint(hash_uint64, 16) != hash {
			return nil, errors.New("Hashes do not match")
		}
		// verify policy request is still valid
		if err := req.Verify(auth); err != nil {
			return nil, err
		}
		return &req, nil

	default:
		return nil, errors.New("Invalid request type: " + t)
	}
}

// delete request, if user is authorized to read resource
func Remove(auth vault.AuthInfo, hash string) error {
	// fetch request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("requests/" + hash)
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("Change ID not found")
	}

	// decode secret to a request
	t := ""
	if raw, ok := resp.Data["Type"]; ok {
		t, ok = raw.(string)
	}
	if t == "" {
		return errors.New("Invalid request type")
	}

	// verify user can access resource
	switch strings.ToLower(t) {
	case "policy":
		// decode secret into policy request
		var req PolicyRequest
		if err := mapstructure.Decode(resp.Data, &req); err != nil {
			return err
		}
		// verify hash
		hash_uint64, err := hashstructure.Hash(req, nil)
		if err != nil || strconv.FormatUint(hash_uint64, 16) != hash {
			return errors.New("Hashes do not match")
		}
		// verify policy request is still valid
		return req.Reject(auth, hash)

	default:
		return errors.New("Invalid request type: " + t)
	}
}

func IsRootOnly(req Request) bool {
	return req.IsRootOnly()
}

// attempts to generate a root token via unseal keys
// will return error if another key generation process is underway
func generateRootToken(unsealKeys []string) (string, error) {
	otp := base64.StdEncoding.EncodeToString(securecookie.GenerateRandomKey(16))
	status, err := vault.GenerateRootInit(otp)
	if err != nil {
		return "", err
	}

	if status.EncodedRootToken == "" {
		for _, s := range unsealKeys {
			status, err = vault.GenerateRootUpdate(s, status.Nonce)
			// an error likely means one of the unseals was not valid
			if err != nil {
				if err2 := vault.GenerateRootCancel(); err2 != nil {
					return "", errors.New("Could not generate root token: " +
						err.Error() + ", " + err2.Error())
				}
			}
		}
	}

	if status.EncodedRootToken == "" {
		return "", errors.New("Could not generate root token. Was vault re-keyed just now?")
	}

	tokenBytes, err := xor.XORBase64(status.EncodedRootToken, otp)
	if err != nil {
		return "", errors.New("Could not decode root token. Please search and revoke")
	}

	token, err := uuid.FormatUUID(tokenBytes)
	if err != nil {
		return "", errors.New("Could not decode root token. Please search and revoke")
	}

	return token, nil
}

// writes the provided unseal in and returns a slice of all unseals in hash
func appendUnseal(hash, unseal string) ([]string, error) {
	// read current request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("unseal_wrapping_tokens/" + hash)
	if err != nil {
		return nil, err
	}

	var wrappingTokens []string

	// if there are already unseals, read them and append
	if resp != nil {
		raw := ""
		if temp, ok := resp.Data["wrapping_tokens"]; ok {
			raw, _ = temp.(string)
		}
		if raw == "" {
			return nil, errors.New("Could not find key 'wrapping_tokens' in cubbyhole")
		}
		wrappingTokens = append(wrappingTokens, strings.Split(raw, ";")...)
	}

	// wrap the unseal token
	newWrappingToken, err := vault.WrapData("60m", map[string]interface{}{
		"unseal_token": unseal,
	})
	if err != nil {
		return nil, err
	}

	// add the new unseal key in
	wrappingTokens = append(wrappingTokens, newWrappingToken)

	// write the unseals back to the cubbyhole
	_, err = vault.WriteToCubbyhole("unseal_wrapping_tokens/"+hash,
		map[string]interface{}{
			"wrapping_tokens": strings.Trim(strings.Join(strings.Fields(fmt.Sprint(wrappingTokens)), ";"), "[]"),
		},
	)
	return wrappingTokens, err
}

func unwrapUnseals(wrappingTokens []string) (unseals []string, err error) {
	for _, wrappingToken := range wrappingTokens {
		data, err := vault.UnwrapData(wrappingToken)
		if err != nil {
			return nil, err
		}
		if unseal, ok := data["unseal_token"]; ok {
			unseals = append(unseals, unseal.(string))
		} else {
			return nil, errors.New("One of the wrapping tokens timed out. Progress reset.")
		}
	}
	return
}
