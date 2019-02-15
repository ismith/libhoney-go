package transmission

import (
	"github.com/honeycombio/libhoney-go/transmission"
)

// WriterOutput implements the Output interface and passes it along to the
// transmission.WriterSender. It is deprecated and you sholud use the
// transmission.WriterSender directly instead. It is provided here for backwards
// compatibility and will be removed eventually.
type WriterOutput struct {
	transmission.WriterSender
}

func (w *WriterOutput) Add(ev *Event) {
	transEv := &transmission.Event{
		APIHost:    ev.APIHost,
		APIKey:     ev.Writekey,
		Dataset:    ev.Dataset,
		SampleRate: ev.SampleRate,
		Timestamp:  ev.Timestamp,
		Metadata:   ev.Metadata,
		Data:       ev.data,
	}
	WriterSender.Add(transEv)
}
