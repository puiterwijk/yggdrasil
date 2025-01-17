package yggdrasil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"git.sr.ht/~spc/go-log"
	"github.com/hashicorp/go-memdb"
)

const (
	// SignalDataProcess is emitted when a data message is processed and ready
	// for dispatching.
	// The value emitted on the channel is the data message's "MessageID" field
	// in the form of a UUIDv4-formatted string.
	SignalDataProcess = "data-process"

	// SignalDataConsume is emitted when data has been consumed. The value
	// emitted on the channel is the data message's "MessageID" field in the
	// form of a UUIDv4-formatted string.
	SignalDataConsume = "data-consume"
)

// DataProcessor converts data messages and prepares them for dispatch.
type DataProcessor struct {
	logger *log.Logger
	sig    signalEmitter
	db     *memdb.MemDB
	client *HTTPClient
}

// NewDataProcessor creates a new data message processor.
func NewDataProcessor(db *memdb.MemDB, certFile string, keyFile string) (*DataProcessor, error) {
	p := new(DataProcessor)
	p.logger = log.New(log.Writer(), fmt.Sprintf("%v[%T] ", log.Prefix(), p), log.Flags(), log.CurrentLevel())

	client, err := NewHTTPClientCertAuth(certFile, keyFile, "")
	if err != nil {
		return nil, err
	}
	p.client = client

	p.db = db

	return p, nil
}

// Connect assigns a channel in the signal table under name for the caller to
// receive event updates.
func (p *DataProcessor) Connect(name string) <-chan interface{} {
	return p.sig.connect(name, 1)
}

// HandleDataRecvSignal receives values on the channel, unpacks the data,
// replaces the content with the contents of the URL if necessary, and emits
// the data on the "data-process" signal.
func (p *DataProcessor) HandleDataRecvSignal(c <-chan interface{}) {
	for e := range c {
		func() {
			var (
				tx  *memdb.Txn
				obj interface{}
				err error
			)

			messageID := e.(string)
			p.logger.Debugf("HandleDataRecvSignal")
			p.logger.Tracef("received value: %#v", messageID)

			tx = p.db.Txn(false)
			obj, err = tx.First(tableNameData, indexNameID, messageID)
			if err != nil {
				p.logger.Error(err)
				return
			}
			dataMessage := obj.(Data)

			tx = p.db.Txn(false)
			obj, err = tx.First(tableNameWorker, indexNameHandler, dataMessage.Directive)
			if err != nil {
				p.logger.Error(err)
				return
			}
			if obj == nil {
				p.logger.Errorf("no worker registered to handle '%v' messages", dataMessage.Directive)
				p.sig.emit(SignalDataConsume, dataMessage.MessageID)
				p.logger.Debugf("emitted signal \"%v\"", SignalDataConsume)
				p.logger.Tracef("emitted value: %#v", dataMessage.MessageID)
				return
			}
			worker := obj.(Worker)

			if worker.detachedContent {
				var URL string
				if err := json.Unmarshal(dataMessage.Content, &URL); err != nil {
					p.logger.Error(err)
					return
				}
				p.logger.Tracef("fetching content from %v", URL)
				resp, err := p.client.Get(URL)
				if err != nil {
					p.logger.Error(err)
					return
				}
				defer resp.Body.Close()

				data, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					p.logger.Error(err)
					return
				}

				switch {
				case resp.StatusCode >= 400:
					p.logger.Error(&APIResponseError{Code: resp.StatusCode, body: strings.TrimSpace(string(data))})
					return
				default:
					p.logger.Infof("received HTTP response body: %v", strings.TrimSpace(string(data)))
					p.logger.Tracef("received HTTP response: %#v", resp)
				}

				dataMessage.Content = data
			}

			tx = p.db.Txn(true)
			if err := tx.Insert(tableNameData, dataMessage); err != nil {
				p.logger.Error(err)
				tx.Abort()
				return
			}
			tx.Commit()

			p.sig.emit(SignalDataProcess, dataMessage.MessageID)
			p.logger.Debugf("emitted signal \"%v\"", SignalDataProcess)
			p.logger.Tracef("emitted value: %#v", dataMessage.MessageID)
		}()
	}
}

// HandleDataReturnSignal receives values on the channel, retrieves the
// originating message, returns the data via HTTP if necessary, removes the
// message from its database and emits the data on the "data-remove" signal.
func (p *DataProcessor) HandleDataReturnSignal(c <-chan interface{}) {
	for e := range c {
		func() {
			var (
				tx  *memdb.Txn
				obj interface{}
				err error
			)

			messageID := e.(string)
			p.logger.Debugf("HandleDataReturnSignal")
			p.logger.Tracef("received value: %#v", messageID)

			tx = p.db.Txn(false)
			obj, err = tx.First(tableNameData, indexNameID, messageID)
			if err != nil {
				p.logger.Error(err)
				return
			}
			dataMessage := obj.(Data)

			tx = p.db.Txn(false)
			obj, err = tx.First(tableNameData, indexNameID, dataMessage.ResponseTo)
			if err != nil {
				p.logger.Error(err)
				return
			}
			originalMessage := obj.(Data)

			tx = p.db.Txn(false)
			obj, err = tx.First(tableNameWorker, indexNameHandler, originalMessage.Directive)
			if err != nil {
				p.logger.Error(err)
				return
			}
			worker := obj.(Worker)

			if worker.detachedContent {
				req, err := http.NewRequest(http.MethodPost, dataMessage.Directive, bytes.NewReader(dataMessage.Content))

				for k, v := range dataMessage.Metadata {
					req.Header.Add(k, strings.TrimSpace(v))
				}
				p.logger.Tracef("created HTTP request: %#v", req)

				resp, err := p.client.Do(req)
				if err != nil {
					p.logger.Error(err)
					return
				}
				defer resp.Body.Close()

				data, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					p.logger.Error(err)
					return
				}

				switch {
				case resp.StatusCode >= 400:
					p.logger.Error(&APIResponseError{Code: resp.StatusCode, body: strings.TrimSpace(string(data))})
					return
				default:
					p.logger.Infof("received HTTP response body: %v", strings.TrimSpace(string(data)))
					p.logger.Tracef("received HTTP response: %#v", resp)
				}
			}

			p.sig.emit(SignalDataConsume, dataMessage.MessageID)
			p.logger.Debugf("emitted signal \"%v\"", SignalDataConsume)
			p.logger.Tracef("emitted value: %#v", dataMessage.MessageID)
		}()
	}
}
