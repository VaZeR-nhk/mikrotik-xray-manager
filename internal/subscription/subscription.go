// Package subscription fetches subscription URLs and parses them into proxies.
package subscription

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"xray-manager/internal/models"
	"xray-manager/internal/parser"
)

var client = &http.Client{Timeout: 15 * time.Second}

type RequestIdentity struct { UserAgent, HWID, DeviceOS, OSVersion, DeviceModel string }

// FetchAndParse downloads and parses a subscription. Empty responses are errors
// so a failed refresh cannot replace a working proxy list with an empty list.
func FetchAndParse(url string, identity RequestIdentity) ([]models.Proxy, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if identity.UserAgent!="" { req.Header.Set("User-Agent",identity.UserAgent) }
 if identity.HWID!="" { req.Header.Set("x-hwid",identity.HWID) }
 if identity.DeviceOS!="" { req.Header.Set("x-device-os",identity.DeviceOS) }
 if identity.OSVersion!="" { req.Header.Set("x-ver-os",identity.OSVersion) }
 if identity.DeviceModel!="" { req.Header.Set("x-device-model",identity.DeviceModel) }

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { return nil,fmt.Errorf("subscription request failed: HTTP %d",resp.StatusCode) }
 if strings.EqualFold(resp.Header.Get("x-hwid-not-supported"),"true"){return nil,fmt.Errorf("Remnawave requires HWID but rejected the supplied identifier; check its format and panel settings")}
 if strings.EqualFold(resp.Header.Get("x-hwid-max-devices-reached"),"true") || strings.EqualFold(resp.Header.Get("x-hwid-limit"),"true"){return nil,fmt.Errorf("Remnawave device limit reached; remove an old device in the panel or increase the user's device limit")}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, err
	}

	text := strings.TrimSpace(string(body))
	// Subscriptions are commonly base64-encoded as a whole; try to decode.
	if decoded, ok := tryBase64(text); ok {
		text = decoded
	}

	var proxies []models.Proxy
	for _, line := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, err := parser.Parse(line)
		if err != nil {
			continue // skip malformed entries
		}
		proxies = append(proxies, p)
	}
	if len(proxies)==0 { return nil,fmt.Errorf("subscription response contained no supported proxy links; configure a compatible Remnawave response format such as XRAY_BASE64") }
	return proxies,nil
}

// tryBase64 returns the decoded text if s decodes cleanly and looks like links.
func tryBase64(s string) (string, bool) {
	clean := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(s)
	clean = strings.ReplaceAll(clean, "-", "+")
	clean = strings.ReplaceAll(clean, "_", "/")
	if m := len(clean) % 4; m != 0 {
		clean += strings.Repeat("=", 4-m)
	}
	dec, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", false
	}
	out := string(dec)
	if strings.Contains(out, "://") {
		return out, true
	}
	return "", false
}
