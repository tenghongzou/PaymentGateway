package kafka

import (
	"google.golang.org/protobuf/proto"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
)

// payloadEventID 盡力從 protobuf payload 取出 event_id（失敗回空字串）。
func payloadEventID(b []byte) string {
	var ev paymentv1.PaymentEvent
	if err := proto.Unmarshal(b, &ev); err != nil {
		return ""
	}
	return ev.GetEventId()
}
