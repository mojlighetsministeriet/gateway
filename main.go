package main // import "github.com/mojlighetsministeriet/gateway"

import (
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/mojlighetsministeriet/gateway/serviceregistry"
	"github.com/mojlighetsministeriet/storage/sessionstore"
	"github.com/mojlighetsministeriet/utils"
	"github.com/mojlighetsministeriet/utils/httprequest"
	"github.com/mojlighetsministeriet/utils/proxy"
	"github.com/mojlighetsministeriet/utils/server"
	uuid "github.com/satori/go.uuid"
)

const internalDefaultURL = "http://gui"
const sessionStorageURL = "http://storage/sessions"
const identityProviderURL = "http://identity-provider"

func main() {
	// Read configuration
	useTLS := true
	if os.Getenv("TLS") == "disable" {
		useTLS = false
	}
	bodyLimit := utils.GetEnv("BODY_LIMIT", "5M")

	// TODO: read as a docker secret instead
	cookieSecret := utils.GetEnv(
		"COOKIE_SECRET",
		strings.Replace(uuid.Must(uuid.NewV4()).String(), "-", "", -1),
	)

	// Create server
	gateway := server.NewServer(useTLS, false, bodyLimit)

	// Create service service
	serviceRegistry, err := serviceregistry.NewServiceRegistry(gateway.Logger)
	if err != nil {
		panic(err)
	}
	serviceRegistry.UpdateFromDockerSocket()

	// Setup session middleware
	sessionStore, err := sessionstore.NewStore(sessionStorageURL, []byte(cookieSecret))
	if err != nil {
		panic(err)
	}
	gateway.Use(session.Middleware(sessionStore))

	// Session management
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

	gateway.Any("/api/*", func(context echo.Context) error {
		url := strings.TrimPrefix(context.Request().URL.String(), "/api/") + "/"
		serviceDomainName := url[:strings.IndexByte(url, '/')]

		if serviceRegistry.Has(serviceDomainName) || strings.HasPrefix(url, "localhost:") {
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
		for serviceName, serviceRoutes := range serviceRegistry.Map() {
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
