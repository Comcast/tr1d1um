package translation

import (
	"net/http"
	"testing"
	"time"

	"github.com/Comcast/webpa-common/wrp"
	"github.com/stretchr/testify/assert"
)

func TestSendWRP(t *testing.T) {
	assert := assert.New(t)

	var (
		contentTypeValue, authHeaderValue string
		sentWRP                           = new(wrp.Message)
	)

	w := NewService(&ServiceOptions{
		XmidtWrpURL: "http://localhost:8090/api/v2",
		CtxTimeout:  time.Second,
		WRPSource:   "local",
		Do:

		//capture sent values of interest
		func(r *http.Request) (resp *http.Response, err error) {
			if err = wrp.NewDecoder(r.Body, wrp.Msgpack).Decode(sentWRP); err == nil {
				contentTypeValue, authHeaderValue = r.Header.Get(contentTypeHeaderKey), r.Header.Get("Authorization")
				resp = &http.Response{Header: http.Header{}}
				return
			}
			return
		},
	})

	wrpMsg := &wrp.Message{
		TransactionUUID: "tid",
		Source:          "test",
	}

	resp, err := w.SendWRP(wrpMsg, "auth")

	assert.Nil(err)
	assert.NotNil(resp)

	//verify correct header values are set in request
	assert.EqualValues(wrp.Msgpack.ContentType(), contentTypeValue)
	assert.EqualValues("auth", authHeaderValue)

	//verify source in WRP message
	assert.EqualValues("local/test", sentWRP.Source)
}
