package main

import (
	"encoding/json"
	"errors"
	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"
	"github.com/julienschmidt/httprouter"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
	"go.uber.org/zap"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	SetStateEvent       = "set state"
	SendServerLogsEvent = "send logs"
	SendCommandEvent    = "send command"
)

type WebsocketMessage struct {
	// The event to perform. Should be one of the following that are supported:
	//
	// - status : Returns the server's power state.
	// - logs : Returns the server log data at the time of the request.
	// - power : Performs a power action aganist the server based the data.
	// - command : Performs a command on a server using the data field.
	Event string `json:"event"`

	// The data to pass along, only used by power/command currently. Other requests
	// should either omit the field or pass an empty value as it is ignored.
	Args []string `json:"args,omitempty"`

	// Is set to true when the request is originating from outside of the Daemon,
	// otherwise set to false for outbound.
	inbound bool
}

type WebsocketHandler struct {
	Server     *server.Server
	Mutex      sync.Mutex
	Connection *websocket.Conn
	JWT        *WebsocketTokenPayload
}

type WebsocketTokenPayload struct {
	jwt.Payload
	UserID      json.Number `json:"user_id"`
	ServerUUID  string      `json:"server_uuid"`
	Permissions []string    `json:"permissions"`
}

const (
	PermissionConnect     = "connect"
	PermissionSendCommand = "send-command"
	PermissionSendPower   = "send-power"
)

// Checks if the given token payload has a permission string.
func (wtp *WebsocketTokenPayload) HasPermission(permission string) bool {
	for _, k := range wtp.Permissions {
		if k == permission {
			return true
		}
	}

	return false
}

var alg *jwt.HMACSHA

// Validates the provided JWT against the known secret for the Daemon and returns the
// parsed data.
//
// This function DOES NOT validate that the token is valid for the connected server, nor
// does it ensure that the user providing the token is able to actually do things.
func ParseJWT(token []byte) (*WebsocketTokenPayload, error) {
	var payload WebsocketTokenPayload
	if alg == nil {
		alg = jwt.NewHS256([]byte(config.Get().AuthenticationToken))
	}

	_, err := jwt.Verify(token, alg, &payload)
	if err != nil {
		return nil, err
	}

	// Check the time of the JWT becoming valid does not exceed more than 15 seconds
	// compared to the system time. This accounts for clock drift to some degree.
	if time.Now().Unix() - payload.NotBefore.Unix() <= -15 {
		return nil, errors.New("jwt violates nbf")
	}

	// Compare the expiration time of the token to the current system time. Include
	// up to 15 seconds of clock drift, and if it has expired return an error and
	// do not process the action.
	if time.Now().Unix() - payload.ExpirationTime.Unix() > 15 {
		return nil, errors.New("jwt violates exp")
	}

	if !payload.HasPermission(PermissionConnect) {
		return nil, errors.New("not authorized to connect to this socket")
	}

	return &payload, nil
}

// Checks if the JWT is still valid.
func (wsh *WebsocketHandler) TokenValid() error {
	if wsh.JWT == nil {
		return errors.New("no jwt present")
	}

	if time.Now().Unix() - wsh.JWT.ExpirationTime.Unix() > 15 {
		return errors.New("jwt violates nbf")
	}

	if !wsh.JWT.HasPermission(PermissionConnect) {
		return errors.New("jwt does not have connect permission")
	}

	if wsh.Server.Uuid != wsh.JWT.ServerUUID {
		return errors.New("jwt server uuid mismatch")
	}

	return nil
}

// Handle a request for a specific server websocket. This will handle inbound requests as well
// as ensure that any console output is also passed down the wire on the socket.
func (rt *Router) routeWebsocket(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	token, err := ParseJWT([]byte(r.URL.Query().Get("token")))
	if err != nil {
		return
	}

	c, err := rt.upgrader.Upgrade(w, r, nil)
	if err != nil {
		zap.S().Error(err)
		return
	}
	defer c.Close()

	s := rt.Servers.Get(ps.ByName("server"))
	handler := WebsocketHandler{
		Server:     s,
		Mutex:      sync.Mutex{},
		Connection: c,
		JWT:        token,
	}

	handleOutput := func(data string) {
		handler.SendJson(&WebsocketMessage{
			Event: server.ConsoleOutputEvent,
			Args:  []string{data},
		})
	}

	handleServerStatus := func(data string) {
		handler.SendJson(&WebsocketMessage{
			Event: server.StatusEvent,
			Args:  []string{data},
		})
	}

	handleResourceUse := func(data string) {
		handler.SendJson(&WebsocketMessage{
			Event: server.StatsEvent,
			Args:  []string{data},
		})
	}

	s.AddListener(server.StatusEvent, &handleServerStatus)
	defer s.RemoveListener(server.StatusEvent, &handleServerStatus)

	s.AddListener(server.ConsoleOutputEvent, &handleOutput)
	defer s.RemoveListener(server.ConsoleOutputEvent, &handleOutput)

	s.AddListener(server.StatsEvent, &handleResourceUse)
	defer s.RemoveListener(server.StatsEvent, &handleResourceUse)

	s.Emit(server.StatusEvent, s.State)

	for {
		j := WebsocketMessage{inbound: true}

		_, p, err := c.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(
				err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
				websocket.CloseServiceRestart,
				websocket.CloseAbnormalClosure,
			) {
				zap.S().Errorw("error handling websocket message", zap.Error(err))
			}
			break
		}

		// Discard and JSON parse errors into the void and don't continue processing this
		// specific socket request. If we did a break here the client would get disconnected
		// from the socket, which is NOT what we want to do.
		if err := json.Unmarshal(p, &j); err != nil {
			continue
		}

		if err := handler.HandleInbound(j); err != nil {
			zap.S().Warnw("error handling inbound websocket request", zap.Error(err))
			break
		}
	}
}

// Perform a blocking send operation on the websocket since we want to avoid any
// concurrent writes to the connection, which would cause a runtime panic and cause
// the program to crash out.
func (wsh *WebsocketHandler) SendJson(v interface{}) error {
	wsh.Mutex.Lock()
	defer wsh.Mutex.Unlock()

	return wsh.Connection.WriteJSON(v)
}

// Handle the inbound socket request and route it to the proper server action.
func (wsh *WebsocketHandler) HandleInbound(m WebsocketMessage) error {
	if !m.inbound {
		return errors.New("cannot handle websocket message, not an inbound connection")
	}

	if err := wsh.TokenValid(); err != nil {
		zap.S().Debugw("jwt token is no longer valid", zap.String("message", err.Error()))

		return nil
	}

	switch m.Event {
	case SetStateEvent:
		{
			if !wsh.JWT.HasPermission(PermissionSendPower) {
				return nil
			}

			var err error
			switch strings.Join(m.Args, "") {
			case "start":
				err = wsh.Server.Environment.Start()
				break
			case "stop":
				err = wsh.Server.Environment.Stop()
				break
			case "restart":
				break
			case "kill":
				err = wsh.Server.Environment.Terminate(os.Kill)
				break
			}

			if err != nil {
				return err
			}
		}
	case SendServerLogsEvent:
		{
			if running, _ := wsh.Server.Environment.IsRunning(); !running {
				return nil
			}

			logs, err := wsh.Server.Environment.Readlog(1024 * 16)
			if err != nil {
				return err
			}

			for _, line := range logs {
				wsh.SendJson(&WebsocketMessage{
					Event: server.ConsoleOutputEvent,
					Args:  []string{line},
				})
			}

			return nil
		}
	case SendCommandEvent:
		{
			if !wsh.JWT.HasPermission(PermissionSendCommand) {
				return nil
			}

			return wsh.Server.Environment.SendCommand(strings.Join(m.Args, ""))
		}
	}

	return nil
}