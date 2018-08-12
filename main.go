package main // import "github.com/mojlighetsministeriet/gateway"

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/purell"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/mojlighetsministeriet/storage/sessionstore"
	"github.com/mojlighetsministeriet/utils"
	"github.com/mojlighetsministeriet/utils/httprequest"
	"github.com/mojlighetsministeriet/utils/proxy"
	"github.com/mojlighetsministeriet/utils/server"
	uuid "github.com/satori/go.uuid"
)

func main() {
	useTLS := true
	if os.Getenv("TLS") == "disable" {
		useTLS = false
	}
	bodyLimit := utils.GetEnv("BODY_LIMIT", "5M")
	cookieSecret := utils.GetEnv(
		"COOKIE_SECRET",
		strings.Replace(uuid.Must(uuid.NewV4()).String(), "-", "", -1),
	)
	internalDefaultURL := purell.MustNormalizeURLString(
		utils.GetEnv("INTERNAL_DEFAULT_URL", "http://gui"),
		purell.FlagsSafe|purell.FlagRemoveTrailingSlash,
	)
	sessionStorageURL := purell.MustNormalizeURLString(
		utils.GetEnv("SESSION_STORAGE_URL", "http://storage/sessions"),
		purell.FlagsSafe|purell.FlagRemoveTrailingSlash,
	)
	identityProviderURL := purell.MustNormalizeURLString(
		utils.GetEnv("IDENTITY_PROVIDER_URL", "http://identity-provider"),
		purell.FlagsSafe|purell.FlagRemoveTrailingSlash,
	)

	gateway := server.NewServer(useTLS, false, bodyLimit)

	sessionStore, err := sessionstore.NewStore(sessionStorageURL, []byte(cookieSecret))
	if err != nil {
		panic(err)
	}
	gateway.Use(session.Middleware(sessionStore))

	gateway.POST("/api/session", func(context echo.Context) error {
		type createTokenBody struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}

		parameters := createTokenBody{}
		context.Bind(&parameters)

		if parameters.Email == "" || parameters.Password == "" {
			return context.JSONBlob(http.StatusUnauthorized, []byte("{\"message\":\"Unauthorized\"}"))
		}

		client, clientError := httprequest.NewJSONClient()
		if clientError != nil {
			gateway.Logger.Error(clientError)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Internal Server Error\"}"))
		}

		type jwt struct {
			Token string `json:"token"`
		}
		token := jwt{}
		requestError := client.Post(identityProviderURL+"/token", &parameters, &token)
		if requestError != nil {
			// TODO: Add error handling, differentiate 4xx and 5xx errors
			gateway.Logger.Error(requestError)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Unauthorized\"}"))
		}

		request := context.Request()
		session, sessionError := sessionStore.Get(request, "session")
		if sessionError != nil {
			gateway.Logger.Error(sessionError)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Internal Server Error\"}"))
		}

		session.Values["token"] = token.Token

		session.Options.HttpOnly = true
		session.Options.Secure = useTLS
		session.Options.Path = "/"

		responseWriter := context.Response().Writer
		err = sessionStore.Save(request, responseWriter, session)
		if err != nil {
			gateway.Logger.Error(err)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Internal Server Error\"}"))
		}

		return context.JSONBlob(http.StatusOK, []byte("{\"message\":\"OK\"}"))
	})

	dockerClient, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	httpClient, err := httprequest.NewJSONClient()
	if err != nil {
		panic(err)
	}

	serviceRegistry := map[string]server.Routes{}

	go func() {
		filters := filters.NewArgs()
		filters.Add("label", "gateway-expose")
		options := types.ServiceListOptions{Filters: filters}

		for {
			services, err := dockerClient.ServiceList(context.Background(), options)
			if err != nil {
				gateway.Logger.Error(err)
				continue
			}

			foundServiceRegistry := map[string]server.Routes{}
			for _, service := range services {
				networks := service.Spec.TaskTemplate.Networks
				for _, network := range networks {
					for _, alias := range network.Aliases {
						routes := server.Routes{}
						routesError := httpClient.Get("http://"+alias+"/help", &routes)
						if routesError == nil {
							foundServiceRegistry[alias] = routes
						} else {
							gateway.Logger.Error(err)
						}
					}
				}
			}
			serviceRegistry = foundServiceRegistry

			time.Sleep(time.Second * 10)
		}
	}()

	gateway.Any("/api/*", func(context echo.Context) error {
		url := strings.TrimPrefix(context.Request().URL.String(), "/api/") + "/"
		serviceDomainName := url[:strings.IndexByte(url, '/')]
		_, serviceFound := serviceRegistry[serviceDomainName]

		if serviceFound {
			return proxy.Request(context, "http://"+url)
		}

		return context.JSONBlob(http.StatusNotFound, []byte("{\"message\":\"Not Found\"}"))
	})

	gateway.GET("*", func(context echo.Context) error {
		url := internalDefaultURL + context.Request().URL.String()
		return proxy.Request(context, url)
	})

	registeredRoutes := server.Routes{}
	gateway.GET("/help", func(context echo.Context) error {
		routes := registeredRoutes
		for serviceName, serviceRoutes := range serviceRegistry {
			for _, serviceRoute := range serviceRoutes {
				serviceRoute.Path = "/api/" + serviceName + serviceRoute.Path
				routes = append(routes, serviceRoute)
			}
		}

		routes.Sort()

		return context.JSON(http.StatusOK, routes)
	})

	for _, route := range gateway.Routes() {
		if !strings.HasSuffix(route.Path, "/*") {
			registeredRoute := server.Route{
				Path:   route.Path,
				Method: route.Method,
			}
			registeredRoutes = append(registeredRoutes, registeredRoute)
		}
	}

	gateway.Listen(":" + utils.GetEnv("PORT", "443"))
}
