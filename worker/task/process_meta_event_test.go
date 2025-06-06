package task

import (
	"context"
	"encoding/json"
	"github.com/frain-dev/convoy/internal/pkg/fflag"
	"github.com/frain-dev/convoy/internal/pkg/tracer"
	"github.com/frain-dev/convoy/pkg/log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/frain-dev/convoy/net"

	"github.com/frain-dev/convoy"
	"github.com/frain-dev/convoy/config"
	"github.com/frain-dev/convoy/datastore"
	"github.com/frain-dev/convoy/mocks"
	"github.com/frain-dev/convoy/queue"
	"github.com/hibiken/asynq"
	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func TestProcessMetaEvent(t *testing.T) {
	tt := []struct {
		name          string
		cfgPath       string
		expectedError error
		msg           *MetaEvent
		dbFn          func(m *mocks.MockMetaEventRepository, p *mocks.MockProjectRepository, l *mocks.MockLicenser)
		nFn           func() func()
	}{
		{
			name:          "Meta Event already sent",
			cfgPath:       "./testdata/Config/basic-convoy.json",
			expectedError: nil,
			msg:           &MetaEvent{MetaEventID: "123", ProjectID: "1234"},
			dbFn: func(m *mocks.MockMetaEventRepository, p *mocks.MockProjectRepository, l *mocks.MockLicenser) {
				l.EXPECT().UseForwardProxy().Times(1).Return(true)
				l.EXPECT().IpRules().Times(1).Return(true)

				m.EXPECT().FindMetaEventByID(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&datastore.MetaEvent{UID: "123", Status: datastore.SuccessEventStatus}, nil)
				p.EXPECT().FetchProjectByID(gomock.Any(), gomock.Any()).
					Return(&datastore.Project{UID: "123"}, nil)
			},
		},

		{
			name:          "Meta Event url does not respond with 2xx",
			cfgPath:       "./testdata/Config/basic-convoy.json",
			expectedError: &EndpointError{Err: ErrMetaEventDeliveryFailed, delay: 20 * time.Second},
			msg:           &MetaEvent{MetaEventID: "123", ProjectID: "1234"},
			dbFn: func(m *mocks.MockMetaEventRepository, p *mocks.MockProjectRepository, l *mocks.MockLicenser) {
				l.EXPECT().UseForwardProxy().Times(1).Return(true)
				l.EXPECT().IpRules().Times(2).Return(true)

				m.EXPECT().FindMetaEventByID(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&datastore.MetaEvent{
						UID: "123",
						Metadata: &datastore.Metadata{
							Data:            []byte(`{"event_type": "endpoint.created"}`),
							Raw:             `{"event_type": "endpoint.created"}`,
							NumTrials:       0,
							RetryLimit:      3,
							IntervalSeconds: 20,
						},
						Status: datastore.ScheduledEventStatus,
					}, nil)
				p.EXPECT().FetchProjectByID(gomock.Any(), gomock.Any()).
					Return(&datastore.Project{
						UID: "123",
						Config: &datastore.ProjectConfig{
							MetaEvent: &datastore.MetaEventConfiguration{},
							SSL:       &datastore.DefaultSSLConfig,
						},
					}, nil)

				m.EXPECT().UpdateMetaEvent(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(2)
			},
			nFn: func() func() {
				httpmock.Activate()

				httpmock.RegisterResponder("POST", "https://google.com",
					httpmock.NewStringResponder(400, ``))

				return func() {
					httpmock.DeactivateAndReset()
				}
			},
		},

		{
			name:          "Max retries reached",
			cfgPath:       "./testdata/Config/basic-convoy.json",
			expectedError: nil,
			msg:           &MetaEvent{MetaEventID: "123", ProjectID: "1234"},
			dbFn: func(m *mocks.MockMetaEventRepository, p *mocks.MockProjectRepository, l *mocks.MockLicenser) {
				l.EXPECT().UseForwardProxy().Times(1).Return(true)
				l.EXPECT().IpRules().Times(2).Return(true)

				m.EXPECT().FindMetaEventByID(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&datastore.MetaEvent{
						UID: "123",
						Metadata: &datastore.Metadata{
							Data:            []byte(`{"event_type": "endpoint.created"}`),
							Raw:             `{"event_type": "endpoint.created"}`,
							NumTrials:       2,
							RetryLimit:      3,
							IntervalSeconds: 20,
						},
						Status: datastore.ScheduledEventStatus,
					}, nil)
				p.EXPECT().FetchProjectByID(gomock.Any(), gomock.Any()).
					Return(&datastore.Project{
						UID: "123",
						Config: &datastore.ProjectConfig{
							MetaEvent: &datastore.MetaEventConfiguration{},
							SSL:       &datastore.DefaultSSLConfig,
						},
					}, nil)

				m.EXPECT().UpdateMetaEvent(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(2)
			},
			nFn: func() func() {
				httpmock.Activate()

				httpmock.RegisterResponder("POST", "https://google.com",
					httpmock.NewStringResponder(400, ``))

				return func() {
					httpmock.DeactivateAndReset()
				}
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			metaEventRepo := mocks.NewMockMetaEventRepository(ctrl)
			projectRepo := mocks.NewMockProjectRepository(ctrl)
			licenser := mocks.NewMockLicenser(ctrl)

			tc.dbFn(metaEventRepo, projectRepo, licenser)

			dispatcher, err := net.NewDispatcher(
				licenser,
				fflag.NewFFlag([]string{string(fflag.IpRules)}),
				net.LoggerOption(log.NewLogger(os.Stdout)),
				net.ProxyOption("nil"),
			)
			require.NoError(t, err)

			err = config.LoadConfig(tc.cfgPath)
			if err != nil {
				t.Errorf("failed to load config file: %v", err)
			}

			if tc.nFn != nil {
				deferFn := tc.nFn()
				defer deferFn()
			}

			processFn := ProcessMetaEvent(projectRepo, metaEventRepo, dispatcher, tracer.NoOpBackend{})
			payload := MetaEvent{
				MetaEventID: tc.msg.MetaEventID,
				ProjectID:   tc.msg.ProjectID,
			}

			data, err := json.Marshal(payload)
			if err != nil {
				t.Errorf("failed to marshal payload: %v", err)
			}

			job := queue.Job{
				Payload: data,
			}

			task := asynq.NewTask(string(convoy.MetaEventProcessor), job.Payload, asynq.Queue(string(convoy.MetaEventQueue)), asynq.ProcessIn(job.Delay))

			err = processFn(context.Background(), task)

			// Asset
			assert.Equal(t, tc.expectedError, err)
		})
	}
}
