package common

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// HandshakeRequest is sent by the client when it first establishes the control stream.
type HandshakeRequest struct {
	Token     string `json:"token"`
	Subdomain string `json:"subdomain"`
}

// HandshakeResponse is sent by the server in response to HandshakeRequest.
type HandshakeResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Token   string `json:"token,omitempty"`
}

// StreamHeader is sent by the server at the beginning of any new multiplexed stream
// to tell the client where to route this connection.
type StreamHeader struct {
	Protocol string `json:"protocol"` // e.g. "http", "tcp"
	Host     string `json:"host"`     // requested host header or subdomain
}

// WriteJSON writes any struct as JSON followed by a newline separator.
func WriteJSON(w io.Writer, val interface{}) error {
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// ReadJSON reads a single newline-terminated JSON message from the reader.
func ReadJSON(r io.Reader, val interface{}) error {
	var buf []byte
	var b [1]byte
	for {
		_, err := r.Read(b[:])
		if err != nil {
			return err
		}
		if b[0] == '\n' {
			break
		}
		buf = append(buf, b[0])
	}
	return json.Unmarshal(buf, val)
}

func GetLocalTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".goinstant", "config.json"), nil
}

func LoadLocalToken() string {
	path, err := GetLocalTokenPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var conf struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &conf); err == nil {
		return conf.Token
	}
	return ""
}

func SaveLocalToken(token string) error {
	path, err := GetLocalTokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	conf := struct {
		Token string `json:"token"`
	}{Token: token}
	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
