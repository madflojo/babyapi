package babyapi

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
)

var respondMtx sync.Mutex

// HTMLer allows for easily represending reponses as HTML strings when accepted content
// type is text/html
type HTMLer interface {
	HTML(*http.Request) string
}

// Create API routes on the given router
func (a *API[T]) Route(r chi.Router) {
	respondMtx.Lock()
	render.Respond = func(w http.ResponseWriter, r *http.Request, v interface{}) {
		if render.GetAcceptedContentType(r) == render.ContentTypeHTML {
			htmler, ok := v.(HTMLer)
			if ok {
				render.HTML(w, r, htmler.HTML(r))
				return
			}
		}

		render.DefaultResponder(w, r, v)
	}
	respondMtx.Unlock()

	for _, m := range a.middlewares {
		r.Use(m)
	}

	if a.parent == nil {
		a.doCustomRoutes(r, a.rootRoutes)
	}

	r.Route(a.base, func(r chi.Router) {
		// Only set these middleware for root-level API
		if a.parent == nil {
			a.defaultMiddleware(r)
		}

		if a.rootAPI {
			for _, subAPI := range a.subAPIs {
				subAPI.Route(r)
			}
			return
		}

		r.With(a.requestBodyMiddleware).Post("/", a.Post)
		r.Get("/", a.GetAll)

		r.With(a.resourceExistsMiddleware).Route(fmt.Sprintf("/{%s}", a.IDParamKey()), func(r chi.Router) {
			for _, m := range a.idMiddlewares {
				r.Use(m)
			}

			r.Get("/", a.Get)
			r.Delete("/", a.Delete)
			r.With(a.requestBodyMiddleware).Put("/", a.Put)
			r.With(a.requestBodyMiddleware).Patch("/", a.Patch)

			for _, subAPI := range a.subAPIs {
				subAPI.Route(r)
			}

			a.doCustomRoutes(r, a.customIDRoutes)
		})

		a.doCustomRoutes(r, a.customRoutes)
	})
}

// Create a new router with API routes
func (a *API[T]) Router() chi.Router {
	r := chi.NewRouter()
	a.Route(r)

	return r
}

func (a *API[T]) doCustomRoutes(r chi.Router, routes []chi.Route) {
	for _, cr := range routes {
		for method, handler := range cr.Handlers {
			r.MethodFunc(method, cr.Pattern, handler.ServeHTTP)
		}
	}
}

func (a *API[T]) defaultGet() http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())

		resource, httpErr := a.GetRequestedResource(r)
		if httpErr != nil {
			logger.Error("error getting requested resource", "error", httpErr.Error())
			return httpErr
		}

		codeOverride, ok := a.customResponseCodes[http.MethodGet]
		if ok {
			render.Status(r, codeOverride)
		}

		return a.responseWrapper(resource)
	})
}

func (a *API[T]) defaultGetAll() http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())

		resources, err := a.Storage.GetAll(a.getAllFilter(r))
		if err != nil {
			logger.Error("error getting resources", "error", err)
			return InternalServerError(err)
		}
		logger.Debug("responding with resources", "count", len(resources))

		var resp render.Renderer
		if a.getAllResponseWrapper != nil {
			resp = a.getAllResponseWrapper(resources)
		} else {
			items := []render.Renderer{}
			for _, item := range resources {
				items = append(items, a.responseWrapper(item))
			}
			resp = &ResourceList[render.Renderer]{Items: items}
		}

		codeOverride, ok := a.customResponseCodes[http.MethodGet]
		if ok {
			render.Status(r, codeOverride)
		}

		return resp
	})
}

func (a *API[T]) defaultPost() http.HandlerFunc {
	return a.ReadRequestBodyAndDo(func(r *http.Request, resource T) (T, *ErrResponse) {
		logger := GetLoggerFromContext(r.Context())

		httpErr := a.onCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		logger.Info("storing resource", "resource", resource)
		err := a.Storage.Set(resource)
		if err != nil {
			logger.Error("error storing resource", "error", err)
			return *new(T), InternalServerError(err)
		}

		codeOverride, ok := a.customResponseCodes[http.MethodPost]
		if ok {
			render.Status(r, codeOverride)
		} else {
			render.Status(r, http.StatusCreated)
		}

		return resource, nil
	})
}

func (a *API[T]) defaultPut() http.HandlerFunc {
	return a.ReadRequestBodyAndDo(func(r *http.Request, resource T) (T, *ErrResponse) {
		logger := GetLoggerFromContext(r.Context())

		if resource.GetID() != a.GetIDParam(r) {
			return *new(T), ErrInvalidRequest(fmt.Errorf("id must match URL path"))
		}

		httpErr := a.onCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		logger.Info("storing resource", "resource", resource)
		err := a.Storage.Set(resource)
		if err != nil {
			logger.Error("error storing resource", "error", err)
			return *new(T), InternalServerError(err)
		}

		codeOverride, ok := a.customResponseCodes[http.MethodPut]
		if ok {
			render.Status(r, codeOverride)
		}

		return resource, nil
	})
}

func (a *API[T]) defaultPatch() http.HandlerFunc {
	return a.ReadRequestBodyAndDo(func(r *http.Request, patchRequest T) (T, *ErrResponse) {
		logger := GetLoggerFromContext(r.Context())

		resource, httpErr := a.GetRequestedResource(r)
		if httpErr != nil {
			logger.Error("error getting requested resource", "error", httpErr.Error())
			return *new(T), httpErr
		}

		patcher, ok := any(resource).(Patcher[T])
		if !ok {
			return *new(T), ErrMethodNotAllowedResponse
		}

		httpErr = patcher.Patch(patchRequest)
		if httpErr != nil {
			logger.Error("error patching resource", "error", httpErr.Error())
			return *new(T), httpErr
		}

		httpErr = a.onCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		logger.Info("storing updated resource", "resource", resource)

		err := a.Storage.Set(resource)
		if err != nil {
			logger.Error("error storing updated resource", "error", err)
			return *new(T), InternalServerError(err)
		}

		codeOverride, ok := a.customResponseCodes[http.MethodPatch]
		if ok {
			render.Status(r, codeOverride)
		}

		return resource, nil
	})
}

func (a *API[T]) defaultDelete() http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())
		httpErr := a.beforeDelete(r)
		if httpErr != nil {
			logger.Error("error executing before func", "error", httpErr)
			return httpErr
		}

		id := a.GetIDParam(r)

		logger.Info("deleting resource", "id", id)

		err := a.Storage.Delete(id)
		if err != nil {
			logger.Error("error deleting resource", "error", err)

			if errors.Is(err, ErrNotFound) {
				return ErrNotFoundResponse
			}

			return InternalServerError(err)
		}

		httpErr = a.afterDelete(r)
		if httpErr != nil {
			logger.Error("error executing after func", "error", httpErr)
			return httpErr
		}

		codeOverride, ok := a.customResponseCodes[http.MethodDelete]
		if ok {
			render.Status(r, codeOverride)
			return nil
		}

		render.NoContent(w, r)
		return nil
	})
}
