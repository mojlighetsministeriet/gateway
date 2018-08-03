package main // import "github.com/mojlighetsministeriet/gateway"

import (
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/purell"
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
	internalDomainSuffix := utils.GetEnv("INTERNAL_DOMAIN_SUFFIX", "localhost")
	internalDefaultURL := purell.MustNormalizeURLString(
		utils.GetEnv("INTERNAL_DEFAULT_URL", "http://gui.localhost"),
		purell.FlagsSafe|purell.FlagRemoveTrailingSlash,
	)
	storageURL := purell.MustNormalizeURLString(
		utils.GetEnv("STORAGE_URL", "http://storage.localhost/sessions"),
		purell.FlagsSafe|purell.FlagRemoveTrailingSlash,
	)
	identityProviderURL := purell.MustNormalizeURLString(
		utils.GetEnv("IDENTITY_PROVIDER_URL", "http://identity-provider.localhost"),
		purell.FlagsSafe|purell.FlagRemoveTrailingSlash,
	)

	server := server.NewServer(useTLS, false, bodyLimit)

	sessionStore, err := sessionstore.NewStore(storageURL, []byte(cookieSecret))
	if err != nil {
		panic(err)
	}
	server.Use(session.Middleware(sessionStore))

	server.POST("/api/session", func(context echo.Context) error {
		type createTokenBody struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}

		parameters := createTokenBody{}
		context.Bind(&parameters)

		if parameters.Email == "" || parameters.Password == "" {
			return context.JSONBlob(http.StatusUnauthorized, []byte("{\"message\":\"Unauthorized\"}"))
		}

		client, err := httprequest.NewJSONClient()
		type jwt struct {
			Token string `json:"token"`
		}
		token := jwt{}
		err = client.Post(identityProviderURL+"/token", &parameters, &token)
		if err != nil {
			// TODO: Add error handling, differentiate 4xx and 5xx errors
			server.Logger.Error(err)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Unauthorized\"}"))
		}

		request := context.Request()
		session, err := sessionStore.Get(request, "session")
		if err != nil {
			server.Logger.Error(err)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Internal Server Error\"}"))
		}

		session.Values["token"] = token.Token

		session.Options.HttpOnly = true
		session.Options.Secure = useTLS
		session.Options.Path = "/"

		responseWriter := context.Response().Writer
		err = sessionStore.Save(request, responseWriter, session)
		if err != nil {
			server.Logger.Error(err)
			return context.JSONBlob(http.StatusInternalServerError, []byte("{\"message\":\"Internal Server Error\"}"))
		}

		return context.JSONBlob(http.StatusOK, []byte("{\"message\":\"OK\"}"))
	})

	addInternalDomainSuffixPattern := regexp.MustCompile("^([^/:]+)(.*)$")
	server.Any("/api/*", func(context echo.Context) error {
		url := strings.TrimPrefix(context.Request().URL.String(), "/api/")
		// TODO: think through if this special check should be here or if slashes always should be added
		if !strings.HasPrefix(url, "localhost:") {
			url = addInternalDomainSuffixPattern.ReplaceAllString(url, "$1."+internalDomainSuffix+"$2")
		}
		return proxy.Request(context, "http://"+url)
	})

	server.GET("*", func(context echo.Context) error {
		url := internalDefaultURL + context.Request().URL.String()
		return proxy.Request(context, url)
	})

	server.Listen(":" + utils.GetEnv("PORT", "443"))
}
