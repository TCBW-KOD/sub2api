// SPDX-License-Identifier: LGPL-3.0-only
package cprnative

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrUsageMissing = errors.New("CPR response has no validated usage; do not record a zero-cost success")
var ErrTerminalMissing = errors.New("CPR SSE ended without a terminal response event")

// Observer reads a single response for accounting; it never supplies relay bytes.
// Memory for each SSE event/JSON document is bounded. No tool adaptation, event
// synthesis, model rewriting or SSE-to-JSON conversion is performed here.
type Observer struct {
	SSE bool
	MaxEventBytes int
	UsageJSON []byte
	ResponseID string
	Model string
	ServiceTier string
	Terminal string
	Failed bool
	SemanticOutput bool
	SkippedEvents int
	ImageCount int
	SearchCount int
	OnJSON func([]byte)
	OnFirstOutput func()
	line []byte
	data []byte
	document []byte
	eventName string
	skipping bool
	lineOverflow bool
	afterCR bool
	firstLine bool
	finished bool
	observationErr error
	imageIDs map[string]bool
	searchIDs map[string]bool
}

func NewObserver(sse bool, limit int) *Observer {
	if limit <= 0 { limit = 16 << 20 }
	return &Observer{SSE:sse, MaxEventBytes:limit, firstLine:true, imageIDs:map[string]bool{}, searchIDs:map[string]bool{}}
}

func (o *Observer) Write(p []byte) (int, error) {
	n := len(p)
	if o.finished { return n, nil }
	if !o.SSE {
		if len(o.document)+len(p) > o.MaxEventBytes { o.skipping=true; o.document=nil; return n,nil }
		if !o.skipping { o.document=append(o.document,p...) }
		return n,nil
	}
	for _, b := range p {
		if o.afterCR { o.afterCR=false; if b=='\n' { continue } }
		if b=='\n' || b=='\r' {
			o.consumeLine(); o.line=o.line[:0]; o.lineOverflow=false; o.afterCR=b=='\r'; continue
		}
		if len(o.line)<o.MaxEventBytes { o.line=append(o.line,b) } else { o.lineOverflow=true }
	}
	return n,nil
}

func (o *Observer) consumeLine() {
	line := o.line
	if o.firstLine { line=bytes.TrimPrefix(line,[]byte{0xef,0xbb,0xbf}); o.firstLine=false }
	if o.lineOverflow { o.skipping=true; o.data=nil; return }
	if len(line)==0 {
		if o.skipping { o.SkippedEvents++ } else if len(o.data)>0 {
			data:=o.data; if data[len(data)-1]=='\n' { data=data[:len(data)-1] }
			if !bytes.Equal(data,[]byte("[DONE]")) { o.observe(data,o.eventName) }
		}
		o.data=o.data[:0]; o.eventName=""; o.skipping=false; return
	}
	if line[0]==':' || o.skipping { return }
	name,value,found:=bytes.Cut(line,[]byte(":")); if !found { value=nil }
	if len(value)>0 && value[0]==' ' { value=value[1:] }
	switch string(name) {
	case "event": o.eventName=string(value)
	case "data":
		if len(o.data)+len(value)+1>o.MaxEventBytes { o.skipping=true; o.data=nil; return }
		o.data=append(o.data,value...); o.data=append(o.data,'\n')
	}
}

func rawString(m map[string]json.RawMessage,k string) string { var s string; _=json.Unmarshal(m[k],&s); return s }
func rawObject(p []byte) map[string]json.RawMessage { var m map[string]json.RawMessage; _=json.Unmarshal(p,&m); return m }

func usageInteger(raw json.RawMessage, name string) (int64,error) {
	if len(raw)==0 || bytes.Equal(bytes.TrimSpace(raw),[]byte("null")) { return 0,ErrUsageMissing }
	var n int64
	if err:=json.Unmarshal(raw,&n); err!=nil || n<0 || n>1<<50 { return 0,fmt.Errorf("invalid usage counter %s",name) }
	return n,nil
}

// Validate all token counters used by the host, not just the headline totals.
// Invalid cache counters must not make an otherwise positive bill negative.
func validateUsage(p []byte) error {
	m:=rawObject(p); if m==nil { return ErrUsageMissing }
	input,err:=usageInteger(m["input_tokens"],"input_tokens"); if err!=nil { return err }
	if _,err=usageInteger(m["output_tokens"],"output_tokens"); err!=nil { return err }
	var inspect func(map[string]json.RawMessage) error
	inspect=func(fields map[string]json.RawMessage) error {
		for key,raw:=range fields {
			if nested:=rawObject(raw); nested!=nil { if err:=inspect(nested); err!=nil { return err }; continue }
			if key=="cached_tokens" || strings.HasSuffix(key,"_tokens") {
				n,err:=usageInteger(raw,key); if err!=nil { return err }
				if (key=="cached_tokens" || key=="cache_read_input_tokens" || key=="cache_read_tokens") && n>input { return errors.New("cached tokens exceed input tokens") }
			}
		}
		return nil
	}
	return inspect(m)
}

func (o *Observer) observe(p []byte,eventName string) {
	if ValidateJSON(p)!=nil { return }
	envelope:=rawObject(p); if envelope==nil { return }
	kind:=rawString(envelope,"type"); if kind=="" { kind=eventName }
	if strings.HasSuffix(kind,".delta") || kind=="response.output_item.done" {
		if !o.SemanticOutput && o.OnFirstOutput!=nil { o.OnFirstOutput() }; o.SemanticOutput=true
	}
	response:=envelope; if nested:=rawObject(envelope["response"]); nested!=nil { response=nested }
	if _,nested:=envelope["response"]; nested || !o.SSE {
		if id:=rawString(response,"id"); id!="" {
			if o.ResponseID!="" && o.ResponseID!=id { o.observationErr=errors.New("multiple response IDs in one HTTP response") }; o.ResponseID=id
		}
		if model:=rawString(response,"model"); model!="" { o.Model=model }
		if tier:=rawString(response,"service_tier"); tier!="" { o.ServiceTier=tier }
	}
	if raw:=response["usage"]; len(raw)>0 && !bytes.Equal(bytes.TrimSpace(raw),[]byte("null")) {
		if err:=validateUsage(raw); err!=nil { o.observationErr=err } else { o.UsageJSON=append(o.UsageJSON[:0],raw...) }
	}
	switch kind {
	case "response.completed","response.done","response.incomplete": o.Terminal=kind
	case "response.failed","error": o.Terminal=kind; o.Failed=true
	}
	if !o.SSE {
		switch rawString(response,"status") {
		case "failed": o.Terminal="response.failed"; o.Failed=true
		case "incomplete": o.Terminal="response.incomplete"
		default: o.Terminal="response.completed"
		}
		if raw,exists:=response["error"]; exists && !bytes.Equal(bytes.TrimSpace(raw),[]byte("null")) { o.Failed=true }
	}
	if kind=="response.output_item.done" { o.observeOutput(rawObject(envelope["item"])) }
	if o.Terminal!="" {
		var outputs []json.RawMessage; _=json.Unmarshal(response["output"],&outputs)
		images,searches:=0,0
		for _,item:=range outputs {
			m:=rawObject(item)
			switch rawString(m,"type") {
			case "image_generation_call": if len(m["result"])>0 && !bytes.Equal(bytes.TrimSpace(m["result"]),[]byte("null")) { images++ }
			case "web_search_call": if rawString(m,"status")=="completed" { searches++ }
			}
		}
		if images>o.ImageCount { o.ImageCount=images }; if searches>o.SearchCount { o.SearchCount=searches }
	}
	if o.OnJSON!=nil { o.OnJSON(p) }
}

func (o *Observer) observeOutput(item map[string]json.RawMessage) {
	id:=rawString(item,"id"); if id=="" { return }
	if len(o.imageIDs)+len(o.searchIDs)>=65536 { o.observationErr=errors.New("too many tool results for accounting observer"); return }
	switch rawString(item,"type") {
	case "image_generation_call":
		if len(item["result"])>0 && !bytes.Equal(bytes.TrimSpace(item["result"]),[]byte("null")) && !o.imageIDs[id] { o.imageIDs[id]=true; o.ImageCount++ }
	case "web_search_call":
		if rawString(item,"status")=="completed" && !o.searchIDs[id] { o.searchIDs[id]=true; o.SearchCount++ }
	}
}

func (o *Observer) HasValidatedUsage() bool { return len(o.UsageJSON)>0 && o.observationErr==nil }

func (o *Observer) Finish() error {
	if !o.finished {
		if !o.SSE { if o.skipping { o.observationErr=errors.New("CPR JSON response exceeds accounting observation limit") } else { o.observe(o.document,"") } }
		o.finished=true; o.line=nil; o.data=nil; o.document=nil
	}
	if o.observationErr!=nil { return o.observationErr }
	if len(o.UsageJSON)==0 { return ErrUsageMissing }
	if o.SSE && o.Terminal=="" { return ErrTerminalMissing }
	return nil
}

// Relay copies original bytes. The host MUST bound accounting drain after the
// client disconnects; onDisconnect lets it shorten the upstream deadline.
func Relay(dst io.Writer,src io.Reader,observer *Observer,flush func() error,onDisconnect func()) (bool,error) {
	buf:=make([]byte,32<<10); disconnected:=false; var writeErr error
	mark:=func(err error) { if !disconnected { disconnected=true; writeErr=err; if onDisconnect!=nil { onDisconnect() } } }
	for {
		n,readErr:=src.Read(buf)
		if n>0 {
			if observer!=nil { _,_=observer.Write(buf[:n]) }
			if !disconnected {
				written,err:=dst.Write(buf[:n]); if err!=nil { mark(err) } else if written!=n { mark(io.ErrShortWrite) }
				if !disconnected && flush!=nil { if err:=flush(); err!=nil { mark(err) } }
			}
		}
		if readErr!=nil { if readErr==io.EOF { return disconnected,writeErr }; return disconnected,errors.Join(writeErr,readErr) }
	}
}
