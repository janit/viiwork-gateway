package auth

import (
	"encoding/json"
	"net/http"
)

// errorBody is OpenAI's error envelope. Clients built against the OpenAI SDK
// parse this; anything else makes them report a transport failure instead of
// the real reason.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// WriteError emits an OpenAI-shaped error response.
func WriteError(w http.ResponseWriter, status int, message, code string) {
	var body errorBody
	body.Error.Message = message
	body.Error.Type = "invalid_request_error"
	body.Error.Code = code

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
