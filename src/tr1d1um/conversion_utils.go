package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/Comcast/webpa-common/wrp"
	"github.com/go-ozzo/ozzo-validation"
)

//Vars shortens frequently used type returned by mux.Vars()
type Vars map[string]string

//ConversionTool lays out the definition of methods to build WDMP from content in an http request
type ConversionTool interface {
	GetFlavorFormat(*http.Request, string, string, string) (*GetWDMP, error)
	SetFlavorFormat(*http.Request) (*SetWDMP, error)
	DeleteFlavorFormat(Vars, string) (*DeleteRowWDMP, error)
	AddFlavorFormat(io.Reader, Vars, string) (*AddRowWDMP, error)
	ReplaceFlavorFormat(io.Reader, Vars, string) (*ReplaceRowsWDMP, error)

	ValidateAndDeduceSET(http.Header, *SetWDMP) error
	GetFromURLPath(string, Vars) (string, bool)
	GetConfiguredWRP([]byte, Vars, http.Header) *wrp.Message
}

//EncodingTool lays out the definition of methods used for encoding/decoding between WDMP and WRP
type EncodingTool interface {
	GenericEncode(interface{}, wrp.Format) ([]byte, error)
	DecodeJSON(io.Reader, interface{}) error
	EncodeJSON(interface{}) ([]byte, error)
	ExtractPayload(io.Reader, wrp.Format) ([]byte, error)
}

//EncodingHelper implements the definitions defined in EncodingTool
type EncodingHelper struct{}

//ConversionWDMP implements the definitions defined in ConversionTool
type ConversionWDMP struct {
	encodingHelper EncodingTool
}

/* The following functions break down the different cases for requests (https://swagger.webpa.comcast.net/)
containing WDMP content. Their main functionality is to attempt at reading such content, validating it
and subsequently returning a json type encoding of it. Most often this resulting []byte is used as payload for
wrp messages
*/
func (cw *ConversionWDMP) GetFlavorFormat(req *http.Request, attr, namesKey, sep string) (wdmp *GetWDMP, err error) {
	wdmp = new(GetWDMP)

	if nameGroup := req.FormValue(namesKey); nameGroup != "" {
		wdmp.Command = CommandGet
		wdmp.Names = strings.Split(nameGroup, sep)
	} else {
		err = errors.New("names is a required property for GET")
		return
	}

	if attributes := req.FormValue(attr); attributes != "" {
		wdmp.Command = CommandGetAttrs
		wdmp.Attribute = attributes
	}

	return
}

func (cw *ConversionWDMP) SetFlavorFormat(req *http.Request) (wdmp *SetWDMP, err error) {
	wdmp = new(SetWDMP)

	if err = cw.encodingHelper.DecodeJSON(req.Body, wdmp); err == nil {
		err = cw.ValidateAndDeduceSET(req.Header, wdmp)
	}
	return
}

func (cw *ConversionWDMP) DeleteFlavorFormat(urlVars Vars, rowKey string) (wdmp *DeleteRowWDMP, err error) {
	wdmp = &DeleteRowWDMP{Command: CommandDeleteRow}

	if row, exists := cw.GetFromURLPath(rowKey, urlVars); exists && row != "" {
		wdmp.Row = row
	} else {
		err = errors.New("non-empty row name is required")
		return
	}
	return
}

func (cw *ConversionWDMP) AddFlavorFormat(input io.Reader, urlVars Vars, tableName string) (wdmp *AddRowWDMP, err error) {
	wdmp = &AddRowWDMP{Command: CommandAddRow}

	if table, exists := cw.GetFromURLPath(tableName, urlVars); exists {
		wdmp.Table = table
	} else {
		err = errors.New("tableName is required for this method")
		return
	}

	if err = cw.encodingHelper.DecodeJSON(input, &wdmp.Row); err == nil {
		err = validation.Validate(wdmp.Row, validation.Required)
	}

	return
}

func (cw *ConversionWDMP) ReplaceFlavorFormat(input io.Reader, urlVars Vars, tableName string) (wdmp *ReplaceRowsWDMP, err error) {
	wdmp = &ReplaceRowsWDMP{Command: CommandReplaceRows}

	if table, exists := cw.GetFromURLPath(tableName, urlVars); exists {
		wdmp.Table = table
	} else {
		err = errors.New("tableName is required for this method")
		return
	}

	if err = cw.encodingHelper.DecodeJSON(input, &wdmp.Rows); err == nil {
		err = validation.Validate(wdmp.Rows, validation.Required)
	}

	return
}

//ValidateAndDeduceSET attempts at defaulting to the SET command given that all the command property requirements are satisfied.
// (name, value, dataType). Then, if the new_cid is provided, it is deduced that the command should be TEST_SET
//else,
func (cw *ConversionWDMP) ValidateAndDeduceSET(header http.Header, wdmp *SetWDMP) (err error) {
	if err = validation.Validate(wdmp.Parameters, validation.Required); err == nil {
		wdmp.Command = CommandSet
		if newCid := header.Get(HeaderWPASyncNewCID); newCid != "" {
			wdmp.OldCid, wdmp.NewCid = header.Get(HeaderWPASyncOldCID), newCid

			if syncCmc := header.Get(HeaderWPASyncCMC); syncCmc != "" {
				wdmp.SyncCmc = syncCmc
			}
			wdmp.Command = CommandTestSet
		}
	} else {
		errMsg := err.Error()
		if !(errMsg == "cannot be blank" || strings.Contains(errMsg, "name")) {
			if err = ValidateSETAttrParams(wdmp.Parameters); err == nil {
				wdmp.Command = CommandSetAttrs
			}
		}
	}
	return
}

//GetFromURLPath Same as invoking urlVars[key] directly but urlVars can be nil in which case key does not exist in it
func (cw *ConversionWDMP) GetFromURLPath(key string, urlVars Vars) (val string, exists bool) {
	if urlVars != nil {
		val, exists = urlVars[key]
	}
	return
}

//GetConfiguredWRP Set the necessary fields in the wrp and return it
func (cw *ConversionWDMP) GetConfiguredWRP(wdmp []byte, pathVars Vars, header http.Header) (wrpMsg *wrp.Message) {
	deviceID, _ := cw.GetFromURLPath("deviceid", pathVars)
	service, _ := cw.GetFromURLPath("service", pathVars)

	wrpMsg = &wrp.Message{
		Type:            wrp.SimpleRequestResponseMessageType,
		ContentType:     header.Get("Content-Type"),
		Payload:         wdmp,
		Source:          WRPSource + "/" + service,
		Destination:     deviceID + "/" + service,
		TransactionUUID: header.Get(HeaderWPATID),
	}
	return
}

/*   Encoding Helper methods below */

//DecodeJSON decodes data from the input into v. It uses json.Unmarshall to perform actual decoding
func (helper *EncodingHelper) DecodeJSON(input io.Reader, v interface{}) (err error) {
	var payload []byte
	if payload, err = ioutil.ReadAll(input); err == nil {
		err = json.Unmarshal(payload, v)
	}
	return
}

//EncodeJSON wraps the json.Marshall method
func (helper *EncodingHelper) EncodeJSON(v interface{}) (data []byte, err error) {
	data, err = json.Marshal(v)
	return
}

//ExtractPayload decodes an encoded wrp message and returns its payload
func (helper *EncodingHelper) ExtractPayload(input io.Reader, format wrp.Format) (payload []byte, err error) {
	wrpResponse := &wrp.Message{}

	if err = wrp.NewDecoder(input, format).Decode(wrpResponse); err == nil {
		payload = wrpResponse.Payload
	}

	return
}

//GenericEncode wraps a WRP encoder. Using a temporary buffer, simply returns the encoded data and error when applicable
func (helper *EncodingHelper) GenericEncode(v interface{}, f wrp.Format) (data []byte, err error) {
	var tmp bytes.Buffer
	err = wrp.NewEncoder(&tmp, f).Encode(v)
	data = tmp.Bytes()
	return
}
