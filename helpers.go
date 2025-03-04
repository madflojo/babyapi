package babyapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
)

// IDParamKey gets the chi URL param key used for the ID of a resource
func IDParamKey(name string) string {
	return fmt.Sprintf("%sID", name)
}

// GetIDParam gets resource ID from the request URL for a resource by name
func GetIDParam(r *http.Request, name string) string {
	return chi.URLParam(r, IDParamKey(name))
}

// GetIDParamFromCtx gets resource ID from the request URL for a resource by name
func GetIDParamFromCtx(ctx context.Context, name string) string {
	return chi.URLParamFromCtx(ctx, IDParamKey(name))
}

// IDParamKey gets the chi URL param key used for this API
func (a *API[T]) IDParamKey() string {
	return IDParamKey(a.name)
}

// GetIDParam gets resource ID from the request URL for this API's resource
func (a *API[T]) GetIDParam(r *http.Request) string {
	param := GetIDParam(r, a.name)
	if param == "" && a.parent != nil {
		param = a.findIDParam(r)
	}
	return param
}

// GetIDParamFromCtx gets resource ID from the request URL for this API's resource
func (a *API[T]) GetIDParamFromCtx(ctx context.Context) string {
	return GetIDParamFromCtx(ctx, a.name)
}

// findIDParam will loop through the whole path to manually find the ID parameter that follows this
// API's base path name. This is used when a parent API has a middleware which applies to child APIs
// and attempts to get the child's ID, but the middleware is not aware of child ID URL parameters
func (a *API[T]) findIDParam(r *http.Request) string {
	index := strings.Index(r.URL.Path, a.base)
	if index == -1 {
		return ""
	}

	result := r.URL.Path[index+len(a.base):]
	result = strings.TrimPrefix(result, "/")

	index = strings.Index(result, "/")
	if index == -1 {
		return result
	}

	result = result[0:index]

	return result
}

// GetRequestedResourceAndDo is a wrapper that handles getting a resource from storage based on the ID in the request URL
// and rendering the response. This is useful for imlementing a CustomIDRoute
func (a *API[T]) GetRequestedResourceAndDo(do func(*http.Request, T) (render.Renderer, *ErrResponse)) http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())

		resource, httpErr := a.GetRequestedResource(r)
		if httpErr != nil {
			logger.Error("error getting requested resource", "error", httpErr.Error())
			return httpErr
		}

		resp, httpErr := do(r, resource)
		if httpErr != nil {
			return httpErr
		}

		if resp == nil {
			render.NoContent(w, r)
			return nil
		}

		return resp
	})
}

// GetRequestedResourceAndDoMiddleware is a shortcut for creating an ID-scoped middleware that gets the requested resource from storage,
// calls the provided 'do' function, and calls next.ServeHTTP. If the resource is not found for a PUT request, the error is ignored
// The 'do' function returns *http.Request so the request context can be modified by middleware
func (a *API[T]) GetRequestedResourceAndDoMiddleware(do func(*http.Request, T) (*http.Request, *ErrResponse)) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			logger := GetLoggerFromContext(r.Context())

			resource, httpErr := a.GetRequestedResource(r)
			if httpErr != nil {
				// Skip for PUT because it can be used to create new resources
				if errors.Is(httpErr, ErrNotFoundResponse) && r.Method == http.MethodPut {
					logger.Warn("resource not found but continuing to next handler")
					next.ServeHTTP(w, r)
					return
				}

				logger.Error("error getting requested resource", "error", httpErr.Error())
				_ = render.Render(w, r, httpErr)
				return
			}

			r, httpErr = do(r, resource)
			if httpErr != nil {
				_ = render.Render(w, r, httpErr)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ReadRequestBodyAndDo is a wrapper that handles decoding the request body into the resource type and rendering a response
func (a *API[T]) ReadRequestBodyAndDo(do func(http.ResponseWriter, *http.Request, T) (T, *ErrResponse)) http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		resource, httpErr := a.GetFromRequest(r)
		if httpErr != nil {
			return httpErr
		}

		resp, httpErr := do(w, r, resource)
		if httpErr != nil {
			return httpErr
		}

		if resp == *new(T) {
			render.NoContent(w, r)
			return nil
		}

		return a.responseWrapper(resp)
	})
}

// ReadRequestBodyAndDo is a helper function that can be used without an API to handle a request
func ReadRequestBodyAndDo[T RendererBinder](do func(http.ResponseWriter, *http.Request, T) (render.Renderer, *ErrResponse), instance func() T) http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		resource, httpErr := GetFromRequest(r, instance)
		if httpErr != nil {
			return httpErr
		}

		resp, httpErr := do(w, r, resource)
		if httpErr != nil {
			return httpErr
		}

		return resp
	})
}

// GetFromRequest will read the API's resource type from the request body or request context
func (a *API[T]) GetFromRequest(r *http.Request) (T, *ErrResponse) {
	return GetFromRequest(r, a.instance)
}

// GetFromRequest will read a resource type from the request body or request context
func GetFromRequest[T RendererBinder](r *http.Request, instance func() T) (T, *ErrResponse) {
	resource, ok := GetRequestBodyFromContext[T](r.Context())
	if ok {
		return resource, nil
	}

	resource = instance()
	err := render.Bind(r, resource)
	if err != nil {
		return *new(T), ErrInvalidRequest(err)
	}

	return resource, nil
}

// GetRequestedResource reads the API's resource from storage based on the ID in the request URL
func (a *API[T]) GetRequestedResource(r *http.Request) (T, *ErrResponse) {
	id := a.GetIDParam(r)

	resource, err := a.Storage.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return *new(T), ErrNotFoundResponse
		}

		return *new(T), InternalServerError(err)
	}

	return resource, nil
}

func Handler(do func(http.ResponseWriter, *http.Request) render.Renderer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := do(w, r)

		if response == nil {
			return
		}
		logger := GetLoggerFromContext(r.Context())

		httpErr, ok := response.(*ErrResponse)
		if ok {
			logger.Error("error returned from handler", "error", httpErr.Err)
		}

		err := render.Render(w, r, response)
		if err != nil {
			logger.Error("unable to render response", "error", err)
			_ = render.Render(w, r, ErrRender(err))
		}
	}
}

// MustRenderHTML renders the provided template and data to a string. Panics if there is an error
func MustRenderHTML(tmpl *template.Template, data any) string {
	var renderedOutput bytes.Buffer
	err := tmpl.Execute(&renderedOutput, data)
	if err != nil {
		panic(err)
	}

	return renderedOutput.String()
}

// MustRenderHTMLMap accepts a map of template name to the template contents. It parses the template
// strings and executes the template with provided data. Since the template map doesn't preserve order,
// the name of the base/root template must be provided. A base *template.Template can be passed in to
// provide custom functions or already-parsed templates. Use nil if nothing is required.
// Panics if there is an error
func MustRenderHTMLMap(tmpl *template.Template, tmplMap map[string]string, name string, data any) string {
	for name, content := range tmplMap {
		if tmpl == nil {
			tmpl = template.Must(template.New(name).Parse(content))
			continue
		}
		tmpl = template.Must(tmpl.New(name).Parse(content))
	}

	var renderedOutput bytes.Buffer
	err := tmpl.ExecuteTemplate(&renderedOutput, name, data)
	if err != nil {
		panic(err)
	}

	return renderedOutput.String()
}
