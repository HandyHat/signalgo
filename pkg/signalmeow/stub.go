package signalmeow

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	signalpb "go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/wspb"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

func Main() {
	provisioning_message := provision_secondary_device()
	aci_public_key, _ := libsignalgo.DeserializePublicKey(provisioning_message.GetAciIdentityKeyPublic())
	aci_private_key, _ := libsignalgo.DeserializePrivateKey(provisioning_message.GetAciIdentityKeyPrivate())
	aci_identity_key_pair, _ := libsignalgo.NewIdentityKeyPair(aci_public_key, aci_private_key)
	pni_public_key, _ := libsignalgo.DeserializePublicKey(provisioning_message.GetPniIdentityKeyPublic())
	pni_private_key, _ := libsignalgo.DeserializePrivateKey(provisioning_message.GetPniIdentityKeyPrivate())
	pni_identity_key_pair, _ := libsignalgo.NewIdentityKeyPair(pni_public_key, pni_private_key)

	log.Printf("aci_identity_key_pair: %v", aci_identity_key_pair)
	log.Printf("pni_identity_key_pair: %v", pni_identity_key_pair)

	username := *provisioning_message.Number
	password, _ := generateRandomPassword(24)
	code := provisioning_message.ProvisioningCode
	registration_id := mrand.Intn(16383) + 1
	pni_registration_id := mrand.Intn(16383) + 1
	device_response := confirm_device(username, password, *code, registration_id, pni_registration_id)
	log.Printf("device_response: %v", device_response)

	if device_response.uuid != "" {
		username = device_response.uuid
	} else {
		username = *provisioning_message.Number
	}
	if device_response.device_id != 0 {
		username = username + "." + fmt.Sprint(device_response.device_id)
	} else {
		username = username + ".1"
	}

	aci_pre_keys := generate_pre_keys(0, 0, 100, aci_identity_key_pair, "aci")
	pni_pre_keys := generate_pre_keys(0, 0, 100, pni_identity_key_pair, "pni")
	register_pre_keys(aci_pre_keys, "aci", username, password)
	register_pre_keys(pni_pre_keys, "pni", username, password)

	// Persist necessary data
}

func generateRandomPassword(length int) (string, error) {
	if length < 1 {
		return "", fmt.Errorf("password length must be at least 1")
	}

	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var password []byte
	for i := 0; i < length; i++ {
		index, err := crand.Int(crand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("error generating random index: %v", err)
		}
		password = append(password, charset[index.Int64()])
	}

	return string(password), nil
}

func open_websocket(ctx context.Context, urlStr string) (*websocket.Conn, *http.Response, error) {
	proxyURL, err := url.Parse("http://localhost:8080")
	if err != nil {
		log.Fatal("Error parsing proxy URL:", err)
	}

	caCertPath := "/Users/sweber/.mitmproxy/mitmproxy-ca-cert.pem"
	caCert, err := ioutil.ReadFile(caCertPath)
	if err != nil {
		log.Fatal("Error reading mitmproxy CA certificate:", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		RootCAs:            caCertPool,
	}

	opt := &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
				Proxy:           http.ProxyURL(proxyURL),
			},
		},
	}
	ws, resp, err := websocket.Dial(ctx, urlStr, opt)

	if err != nil {
		log.Printf("failed on open %v", resp)
		log.Fatal(err)
	}
	return ws, resp, err
}

func provision_secondary_device() *signalpb.ProvisionMessage {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ws, resp, err := open_websocket(ctx, "wss://chat.signal.org:443/v1/websocket/provisioning/")
	defer ws.Close(websocket.StatusInternalError, "Websocket StatusInternalError")

	provisioning_cipher := NewProvisioningCipher()
	pub_key := provisioning_cipher.GetPublicKey()

	// The things we want
	provisioning_url := ""
	envelope := &signalpb.ProvisionEnvelope{}

	msg := &signalpb.WebSocketMessage{}
	err = wspb.Read(ctx, ws, msg)
	if err != nil {
		log.Printf("failed on read %v", resp)
		log.Fatal(err)
	}
	log.Printf("*** Received: %s", msg)

	// Ensure the message is a request and has a valid verb and path
	if *msg.Type == signalpb.WebSocketMessage_REQUEST &&
		*msg.Request.Verb == "PUT" &&
		*msg.Request.Path == "/v1/address" {

		// Decode provisioning UUID
		provisioning_uuid := &signalpb.ProvisioningUuid{}
		err = proto.Unmarshal(msg.Request.Body, provisioning_uuid)

		// Create provisioning URL
		bytes_key, _ := pub_key.Serialize()
		base64_key := base64.StdEncoding.EncodeToString(bytes_key)
		uuid := url.QueryEscape(*provisioning_uuid.Uuid)
		pub_key := url.QueryEscape(base64_key)
		provisioning_url = "sgnl://linkdevice?uuid=" + uuid + "&pub_key=" + pub_key
		log.Printf("provisioning_url: %s", provisioning_url)

		// Create a 200 response
		msg_type := signalpb.WebSocketMessage_RESPONSE
		message := "OK"
		status := uint32(200)
		response := &signalpb.WebSocketMessage{
			Type: &msg_type,
			Response: &signalpb.WebSocketResponseMessage{
				Id:      msg.Request.Id,
				Message: &message,
				Status:  &status,
				Headers: []string{},
			},
		}

		// Send response
		err = wspb.Write(ctx, ws, response)
		if err != nil {
			log.Printf("failed on write %v", resp)
			log.Fatal(err)
		}

		log.Printf("*** Sent: %s", response)
	}

	// Print the provisioning URL to the console as a QR code
	qrterminal.Generate(provisioning_url, qrterminal.M, os.Stdout)

	msg2 := &signalpb.WebSocketMessage{}
	err = wspb.Read(ctx, ws, msg2)
	if err != nil {
		log.Printf("failed on 2nd read %v", resp)
		log.Fatal(err)
	}
	log.Printf("*** Received: %s", msg2)

	if *msg2.Type == signalpb.WebSocketMessage_REQUEST &&
		*msg2.Request.Verb == "PUT" &&
		*msg2.Request.Path == "/v1/message" {

		envelope = &signalpb.ProvisionEnvelope{}
		err = proto.Unmarshal(msg2.Request.Body, envelope)
		if err != nil {
			log.Printf("failed on unmarshal %v", resp)
			log.Fatal(err)
		}

		// Create a 200 response
		msg_type := signalpb.WebSocketMessage_RESPONSE
		message := "OK"
		status := uint32(200)
		response := &signalpb.WebSocketMessage{
			Type: &msg_type,
			Response: &signalpb.WebSocketResponseMessage{
				Id:      msg2.Request.Id,
				Message: &message,
				Status:  &status,
				Headers: []string{},
			},
		}

		// Send response
		err = wspb.Write(ctx, ws, response)
		if err != nil {
			log.Printf("failed on write %v", resp)
			log.Fatal(err)
		}
		log.Printf("*** Sent: %s", response)
	}

	ws.Close(websocket.StatusNormalClosure, "")

	log.Printf("provisioning_url: %v", provisioning_url)
	log.Printf("Envelope: %v", envelope)
	provisioning_message := provisioning_cipher.Decrypt(envelope)
	log.Printf("provisioning_message: %v", provisioning_message)

	return provisioning_message
}

type ConfirmDeviceResponse struct {
	uuid      string
	pni       string
	device_id int
}

func confirm_device(username string, password string, code string, registration_id int, pni_registration_id int) ConfirmDeviceResponse {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ws, resp, err := open_websocket(ctx, "wss://chat.signal.org:443/v1/websocket/")
	defer ws.Close(websocket.StatusInternalError, "Websocket StatusInternalError")

	data := map[string]interface{}{
		"registrationId":    registration_id,
		"pniRegistrationId": pni_registration_id,
		"supportsSms":       true,
	}
	// TODO: Set deviceName with "Signal Bridge" or something properly encrypted

	json_bytes, err := json.Marshal(data)
	if err != nil {
		log.Fatal(err)
	}

	msg_type := signalpb.WebSocketMessage_REQUEST
	response := &signalpb.WebSocketMessage{
		Type: &msg_type,
		Request: &signalpb.WebSocketRequestMessage{
			Id:   proto.Uint64(1),
			Verb: proto.String("PUT"),
			Path: proto.String("/v1/devices/" + code),
			Body: json_bytes,
		},
	}
	response.Request.Headers = append(response.Request.Headers, "Content-Type: application/json")
	basicAuth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	response.Request.Headers = append(response.Request.Headers, "authorization:Basic "+basicAuth)

	// Send response
	err = wspb.Write(ctx, ws, response)
	if err != nil {
		log.Printf("failed on write %v", resp)
		log.Fatal(err)
	}

	log.Printf("*** Sent: %s", response)

	received_msg := &signalpb.WebSocketMessage{}
	err = wspb.Read(ctx, ws, received_msg)
	if err != nil {
		log.Printf("failed to read after devices call: %v", resp)
		log.Fatal(err)
	}
	log.Printf("*** Received: %s", received_msg)

	// Decode body into JSON
	var body map[string]interface{}
	err = json.Unmarshal(received_msg.Response.Body, &body)
	if err != nil {
		log.Fatal(err)
	}

	// Put body into struct
	device_resp := ConfirmDeviceResponse{}
	uuid, ok := body["uuid"].(string)
	if ok {
		device_resp.uuid = uuid
	}
	pni, ok := body["pni"].(string)
	if ok {
		device_resp.pni = pni
	}
	deviceId, ok := body["deviceId"].(float64)
	if ok {
		device_resp.device_id = int(deviceId)
	}

	return device_resp
}

type GeneratedPreKeys struct {
	PreKeys      []*libsignalgo.PreKeyRecord
	SignedPreKey *libsignalgo.SignedPreKeyRecord
	IdentityKey  []uint8
}

func generate_pre_keys(start_key_id uint32, start_signed_key_id uint32, count uint32, identity_key_pair *libsignalgo.IdentityKeyPair, uuid_kind string) *GeneratedPreKeys {
	// Generate n prekeys
	generated_pre_keys := &GeneratedPreKeys{}
	for i := start_key_id; i < start_key_id+count; i++ {
		private_key, err := libsignalgo.GeneratePrivateKey()
		if err != nil {
			log.Fatalf("Error generating private key: %v", err)
		}
		pre_key, err := libsignalgo.NewPreKeyRecordFromPrivateKey(i, private_key)
		if err != nil {
			log.Fatalf("Error creating preKey record: %v", err)
		}
		generated_pre_keys.PreKeys = append(generated_pre_keys.PreKeys, pre_key)
	}

	// Generate a signed prekey
	private_key, err := libsignalgo.GeneratePrivateKey()
	if err != nil {
		log.Fatalf("Error generating private key: %v", err)
	}
	timestamp := time.Now()
	public_key, err := private_key.GetPublicKey()
	if err != nil {
		log.Fatalf("Error getting public key: %v", err)
	}
	serialized_public_key, err := public_key.Serialize()
	if err != nil {
		log.Fatalf("Error serializing public key: %v", err)
	}
	signature, err := identity_key_pair.GetPrivateKey().Sign(serialized_public_key)
	if err != nil {
		log.Fatalf("Error signing public key: %v", err)
	}
	generated_pre_keys.SignedPreKey = &libsignalgo.SignedPreKeyRecord{}
	generated_pre_keys.SignedPreKey, err = libsignalgo.NewSignedPreKeyRecordFromPrivateKey(start_signed_key_id, timestamp, private_key, signature)

	// Save identity key
	identity_key, err := identity_key_pair.GetPublicKey().Serialize()
	if err != nil {
		log.Fatalf("Error serializing identity key: %v", err)
	}
	generated_pre_keys.IdentityKey = identity_key

	return generated_pre_keys
}

func register_pre_keys(generated_pre_keys *GeneratedPreKeys, uuid_kind string, username string, password string) {
	// Convert generated prekeys to JSON
	pre_keys_json := []map[string]interface{}{}
	for _, pre_key := range generated_pre_keys.PreKeys {
		id, _ := pre_key.GetID()
		publicKey, _ := pre_key.GetPublicKey()
		serialized_key, _ := publicKey.Serialize()
		pre_key_json := map[string]interface{}{
			"keyId":     id,
			"publicKey": base64.StdEncoding.EncodeToString(serialized_key),
		}
		pre_keys_json = append(pre_keys_json, pre_key_json)
	}

	// Convert signed prekey to JSON
	id, _ := generated_pre_keys.SignedPreKey.GetID()
	publicKey, _ := generated_pre_keys.SignedPreKey.GetPublicKey()
	serialized_key, _ := publicKey.Serialize()
	signature, _ := generated_pre_keys.SignedPreKey.GetSignature()
	signed_pre_key_json := map[string]interface{}{
		"keyId":     id,
		"publicKey": serialized_key,
		"signature": base64.StdEncoding.EncodeToString(signature),
	}
	identity_key := generated_pre_keys.IdentityKey
	register_json := map[string]interface{}{
		"preKeys":      pre_keys_json,
		"signedPreKey": signed_pre_key_json,
		"identityKey":  base64.StdEncoding.EncodeToString(identity_key),
	}

	// Send request
	keys_url := "https://chat.signal.org/v2/keys?identity=" + uuid_kind
	json_bytes, err := json.Marshal(register_json)
	if err != nil {
		log.Fatalf("Error marshalling register JSON: %v", err)
	}
	req, err := http.NewRequest("PUT", keys_url, bytes.NewBuffer(json_bytes))
	if err != nil {
		log.Fatalf("Error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	//req.Header.Set("User-Agent", "SignalBridge/0.1")
	//req.Header.Set("X-Signal-Agent", "SignalBridge/0.1")
	req.SetBasicAuth(username, password)

	proxyURL, err := url.Parse("http://localhost:8080")
	if err != nil {
		log.Fatal("Error parsing proxy URL:", err)
	}

	caCertPath := "/Users/sweber/.mitmproxy/mitmproxy-ca-cert.pem"
	caCert, err := ioutil.ReadFile(caCertPath)
	if err != nil {
		log.Fatal("Error reading mitmproxy CA certificate:", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		RootCAs:            caCertPool,
	}
	client := &http.Client{}
	client.Transport = &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: tlsConfig,
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Error sending request: %v", err)
	}
	defer resp.Body.Close()
	log.Printf("Response status: %s", resp.Status)
	log.Printf("Response headers: %s", resp.Header)
	body, _ := ioutil.ReadAll(resp.Body)
	log.Printf("Response body: %s", body)
}
