package hormuud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buger/jsonparser"
	"github.com/garyburd/redigo/redis"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	maxMsgLength = 160
	tokenURL     = "https://smsapi.hormuud.com/token"
	sendURL      = "https://smsapi.hormuud.com/api/SendSMS"
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("HM"), "Hormuud")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveMessage)
	return nil
}

type moPayload struct {
	Sender      string `validate:"required"`
	MessageText string
	ShortCode   string `validate:"required"`
	TimeSent    int64  `validate:"required"`
}

// receiveMessage is our HTTP handler function for incoming messages
func (h *handler) receiveMessage(ctx context.Context, c courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	payload := &moPayload{}
	err := handlers.DecodeAndValidateForm(payload, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, c, w, r, err)
	}

	// create our date from the timestamp
	date := time.Unix(payload.TimeSent, 0).UTC()

	urn, err := handlers.StrictTelForCountry(payload.Sender, c.Country())
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, c, w, r, err)
	}

	msg := h.Backend().NewIncomingMsg(c, urn, payload.MessageText).WithReceivedOn(date)
	return handlers.WriteMsgsAndResponse(ctx, h, []courier.Msg{msg}, w, r)
}

type mtPayload struct {
	Mobile   string `json:"mobile"`
	Message  string `json:"message"`
	SenderID string `json:"senderid"`
	MType    int    `json:"mType"`
	EType    int    `json:"eType"`
	UDH      string `json:"UDH"`
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)

	token, rr, err := h.FetchToken(ctx, msg.Channel(), msg)
	if rr == nil && err != nil {
		return nil, errors.Wrapf(err, "unable to fetch token")
	}

	// if we made a request for our token, stash that in our status
	if rr != nil {
		log := courier.NewChannelLogFromRR("Token Retrieved", msg.Channel(), msg.ID(), rr).WithError("Token Retrieval Error", err)
		status.AddLog(log)
	}

	// failed getting a token? we are done
	if err != nil {
		return status, nil
	}

	parts := handlers.SplitMsgByChannel(msg.Channel(), handlers.GetTextAndAttachments(msg), maxMsgLength)
	for i, part := range parts {
		payload := &mtPayload{}
		payload.Mobile = strings.TrimPrefix(msg.URN().Path(), "+")
		payload.Message = part
		payload.SenderID = msg.Channel().Address()
		payload.MType = -1
		payload.EType = -1
		payload.UDH = ""

		requestBody := &bytes.Buffer{}
		json.NewEncoder(requestBody).Encode(payload)

		// build our request
		req, err := http.NewRequest(http.MethodPost, sendURL, requestBody)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

		if err != nil {
			courier.LogRequestError(req, msg.Channel(), err)
		}

		rr, err := utils.MakeHTTPRequest(req)
		log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)
		status.AddLog(log)
		if err != nil {
			return status, nil
		}
		status.SetStatus(courier.MsgWired)

		// try to get the message id out
		id, _ := jsonparser.GetString(rr.Body, "Data", "MessageID")
		if id != "" && i == 0 {
			status.SetExternalID(id)
		}
	}

	return status, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token" validate:"required"`
}

// FetchToken gets the current token for this channel, either from Redis if cached or by requesting it
func (h *handler) FetchToken(ctx context.Context, channel courier.Channel, msg courier.Msg) (string, *utils.RequestResponse, error) {
	// first check whether we have it in redis
	conn := h.Backend().RedisPool().Get()
	token, err := redis.String(conn.Do("GET", fmt.Sprintf("hm_token_%s", channel.UUID())))
	conn.Close()

	// got a token, use it
	if token != "" {
		return token, nil, nil
	}

	// no token, lets go fetch one
	username := channel.StringConfigForKey(courier.ConfigUsername, "")
	if username == "" {
		return "", nil, fmt.Errorf("Missing 'username' config for HM channel")
	}

	password := channel.StringConfigForKey(courier.ConfigPassword, "")
	if password == "" {
		return "", nil, fmt.Errorf("Missing 'password' config for HM channel")
	}

	form := url.Values{
		"Username":   []string{username},
		"Password":   []string{password},
		"grant_type": []string{"password"},
	}

	// build our request
	req, _ := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	rr, err := utils.MakeHTTPRequest(req)
	if err != nil {
		return "", rr, errors.Wrapf(err, "error making token request")
	}

	token, err = jsonparser.GetString(rr.Body, "access_token")
	if err != nil {
		return "", rr, errors.Wrapf(err, "error getting access_token from response")
	}

	if token == "" {
		return "", rr, errors.Errorf("no access token returned")
	}

	// we got a token, cache it to redis with a 90 minute expiration
	conn = h.Backend().RedisPool().Get()
	_, err = conn.Do("SETEX", fmt.Sprintf("hm_token_%s", channel.UUID()), 5340, token)
	conn.Close()

	if err != nil {
		logrus.WithError(err).Error("error caching HM access token")
	}

	return token, rr, nil
}
