package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/frain-dev/convoy/api/models"
	"github.com/frain-dev/convoy/database/postgres"
	"github.com/frain-dev/convoy/datastore"
	"github.com/frain-dev/convoy/pkg/log"
	"github.com/frain-dev/convoy/util"
	"github.com/go-chi/render"

	"github.com/frain-dev/convoy/internal/pkg/middleware"
)

func (h *Handler) GetDashboardSummary(w http.ResponseWriter, r *http.Request) {
	format := "2006-01-02T15:04:05"
	startDate := r.URL.Query().Get("startDate")
	endDate := r.URL.Query().Get("endDate")
	if len(startDate) == 0 {
		_ = render.Render(w, r, util.NewErrorResponse("please specify a startDate query", http.StatusBadRequest))
		return
	}

	startT, err := time.Parse(format, startDate)
	if err != nil {
		h.A.Logger.WithError(err).Error("error parsing startDate")
		_ = render.Render(w, r, util.NewErrorResponse("please specify a startDate in the format "+format, http.StatusBadRequest))
		return
	}

	period := r.URL.Query().Get("type")
	if util.IsStringEmpty(period) {
		_ = render.Render(w, r, util.NewErrorResponse("please specify a type query", http.StatusBadRequest))
		return
	}

	if !datastore.IsValidPeriod(period) {
		_ = render.Render(w, r, util.NewErrorResponse("please specify a type query in (daily, weekly, monthly, yearly)", http.StatusBadRequest))
		return
	}

	var endT time.Time
	if len(endDate) == 0 {
		endT = time.Date(startT.Year(), startT.Month(), startT.Day(), 23, 59, 59, 999999999, startT.Location())
	} else {
		endT, err = time.Parse(format, endDate)
		if err != nil {
			_ = render.Render(w, r, util.NewErrorResponse("please specify an endDate in the format "+format+" or none at all", http.StatusBadRequest))
			return
		}
	}

	p := datastore.PeriodValues[period]
	if err = middleware.EnsurePeriod(startT, endT); err != nil {
		_ = render.Render(w, r, util.NewErrorResponse(fmt.Sprintf("invalid period '%s': %s", period, err.Error()), http.StatusBadRequest))
		return
	}

	searchParams := datastore.SearchParams{
		CreatedAtStart: startT.Unix(),
		CreatedAtEnd:   endT.Unix(),
	}

	project, err := h.retrieveProject(r)
	if err != nil {
		_ = render.Render(w, r, util.NewServiceErrResponse(err))
		return
	}

	endpointIDs := make([]string, 0)
	authUser := middleware.GetAuthUserFromContext(r.Context())
	pLQ := ""
	if h.IsReqWithPortalLinkToken(authUser) {
		portalLink, innerErr := h.retrievePortalLinkFromToken(r)
		if innerErr != nil {
			_ = render.Render(w, r, util.NewServiceErrResponse(innerErr))
			return
		}
		pLQ = ":" + portalLink.UID

		eIDs, innerErr := h.getEndpoints(r, portalLink)
		if innerErr != nil {
			_ = render.Render(w, r, util.NewServiceErrResponse(innerErr))
			return
		}

		if len(eIDs) == 0 {
			intervals := make([]datastore.EventInterval, 0)
			dashboardPL := models.DashboardSummary{
				Applications: 0,
				EventsSent:   0,
				Period:       period,
				PeriodData:   &intervals,
			}

			_ = render.Render(w, r, util.NewServerResponse("Dashboard summary fetched successfully.",
				dashboardPL, http.StatusOK))
			return
		}

		endpointIDs = append(endpointIDs, eIDs...)
	}

	qs := fmt.Sprintf("%v:%v:%v:%v%v", project.UID, searchParams.CreatedAtStart, searchParams.CreatedAtEnd, period, pLQ)

	var data *models.DashboardSummary
	err = h.A.Cache.Get(r.Context(), qs, &data)
	if err != nil {
		h.A.Logger.WithError(err).Error("failed to get dashboard summary from cache")
	}

	if data != nil {
		h.cacheNewDashboardDataInBackground(project, searchParams, p, period, qs, endpointIDs)
		_ = render.Render(w, r, util.NewServerResponse("Dashboard summary fetched successfully",
			data, http.StatusOK))
		return
	}

	var endpoints int64
	if len(endpointIDs) == 0 {
		endpoints, err = postgres.NewEndpointRepo(h.A.DB).CountProjectEndpoints(r.Context(), project.UID)
		if err != nil {
			log.WithError(err).Error("failed to count project endpoints")
			_ = render.Render(w, r, util.NewErrorResponse("an error occurred while searching apps", http.StatusInternalServerError))
			return
		}
	} else {
		endpoints = int64(len(endpointIDs))
	}

	eventsSent, messages, err := h.computeDashboardMessages(r.Context(), project.UID, searchParams, p, endpointIDs)
	if err != nil {
		_ = render.Render(w, r, util.NewErrorResponse("an error occurred while fetching messages", http.StatusInternalServerError))
		return
	}

	dashboard := models.DashboardSummary{
		Applications: int(endpoints),
		EventsSent:   eventsSent,
		Period:       period,
		PeriodData:   &messages,
	}

	err = h.A.Cache.Set(r.Context(), qs, dashboard, time.Hour)

	if err != nil {
		h.A.Logger.WithError(err)
	}

	_ = render.Render(w, r, util.NewServerResponse("Dashboard summary fetched successfully",
		dashboard, http.StatusOK))
}

func (h *Handler) cacheNewDashboardDataInBackground(project *datastore.Project, searchParams datastore.SearchParams, p datastore.Period, period string, qs string, endpointIds []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	qsQuery := qs + ":query"
	var dashboardQ *models.DashboardSummary
	_ = h.A.Cache.Get(ctx, qsQuery, &dashboardQ)
	if dashboardQ != nil {
		log.Warn("Query still running in a Goroutine")
		return
	}

	go func() {
		dashboardQ = &models.DashboardSummary{}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		err := h.A.Cache.Set(ctx, qsQuery, dashboardQ, 2*time.Minute)
		if err != nil {
			h.A.Logger.WithError(err).Error("failed to cache query item: " + qsQuery)
			return
		}

		var endpoints int64
		if len(endpointIds) == 0 {
			endpoints, err = postgres.NewEndpointRepo(h.A.DB).CountProjectEndpoints(ctx, project.UID)
			if err != nil {
				log.WithError(err).Error("failed to count project endpoints")
				return
			}
		} else {
			endpoints = int64(len(endpointIds))
		}
		eventsSent, messages, err := h.computeDashboardMessages(ctx, project.UID, searchParams, p, endpointIds)
		if err != nil {
			log.WithError(err).Error("an error occurred while fetching messages")
			return
		}

		dashboard := models.DashboardSummary{
			Applications: int(endpoints),
			EventsSent:   eventsSent,
			Period:       period,
			PeriodData:   &messages,
		}

		err = h.A.Cache.Set(ctx, qs, dashboard, time.Hour)
		if err != nil {
			h.A.Logger.WithError(err).Error("failed to cache item")
		}

		err = h.A.Cache.Delete(ctx, qsQuery)
		if err != nil {
			h.A.Logger.WithError(err).Error("failed to delete cache item")
		}
	}()
}

func (h *Handler) computeDashboardMessages(ctx context.Context, projectID string, searchParams datastore.SearchParams, period datastore.Period, endpointIds []string) (uint64, []datastore.EventInterval, error) {
	var messagesSent uint64

	eventDeliveryRepo := postgres.NewEventDeliveryRepo(h.A.DB)
	messages, err := eventDeliveryRepo.LoadEventDeliveriesIntervals(ctx, projectID, searchParams, period, endpointIds)
	if err != nil {
		log.FromContext(ctx).WithError(err).Error("failed to load message intervals - ")
		return 0, nil, err
	}

	for _, m := range messages {
		messagesSent += m.Count
	}

	return messagesSent, messages, nil
}
