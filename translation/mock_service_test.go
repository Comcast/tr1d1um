// Code generated by mockery v2.6.0. DO NOT EDIT.

package translation

import (
	context "context"

	common "github.com/xmidt-org/tr1d1um/common"

	mock "github.com/stretchr/testify/mock"

	wrp "github.com/xmidt-org/wrp-go/v3"
)

// MockService is an autogenerated mock type for the Service type
type MockService struct {
	mock.Mock
}

// SendWRP provides a mock function with given fields: _a0, _a1, _a2
func (_m *MockService) SendWRP(_a0 context.Context, _a1 *wrp.Message, _a2 string) (*common.XmidtResponse, error) {
	ret := _m.Called(_a0, _a1, _a2)

	var r0 *common.XmidtResponse
	if rf, ok := ret.Get(0).(func(context.Context, *wrp.Message, string) *common.XmidtResponse); ok {
		r0 = rf(_a0, _a1, _a2)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*common.XmidtResponse)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, *wrp.Message, string) error); ok {
		r1 = rf(_a0, _a1, _a2)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}
