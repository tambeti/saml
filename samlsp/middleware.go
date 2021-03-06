package samlsp

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"

	"github.com/tambeti/saml"
)

// Middleware implements middleware than allows a web application
// to support SAML.
//
// It implements http.Handler so that it can provide the metadata and ACS endpoints,
// typically /saml/metadata and /saml/acs, respectively.
//
// It also provides middleware, RequireAccount which redirects users to
// the auth process if they do not have session credentials.
//
// When redirecting the user through the SAML auth flow, the middlware assigns
// a temporary cookie with a random name beginning with "saml_". The value of
// the cookie is a signed JSON Web Token containing the original URL requested
// and the SAML request ID. The random part of the name corresponds to the
// RelayState parameter passed through the SAML flow.
//
// When validating the SAML response, the RelayState is used to look up the
// correct cookie, validate that the SAML request ID, and redirect the user
// back to their original URL.
//
// Sessions are established by issuing a JSON Web Token (JWT) as a session
// cookie once the SAML flow has succeeded. The JWT token contains the
// authenticated attributes from the SAML assertion.
//
// When the middlware receives a request with a valid session JWT it extracts
// the SAML attributes and modifies the http.Request object adding headers
// corresponding to the specified attributes. For example, if the attribute
// "cn" were present in the initial assertion with a value of "Alice Smith",
// then a corresponding header "X-Saml-Cn" will be added to the request with
// a value of "Alice Smith". For safety, the middleware strips out any existing
// headers that begin with "X-Saml-".
//
// When issuing JSON Web Tokens, a signing key is required. Because the
// SAML service provider already has a private key, we borrow that key
// to sign the JWTs as well.
type Middleware struct {
	ServiceProvider   saml.ServiceProvider
	AllowIDPInitiated bool
}

const cookieMaxAge = time.Hour // TODO(ross): must be configurable
const cookieName = "token"

func randomBytes(n int) []byte {
	rv := make([]byte, n)
	if _, err := saml.RandReader.Read(rv); err != nil {
		panic(err)
	}
	return rv
}

// ServeHTTP implements http.Handler and serves the SAML-specific HTTP endpoints
// on the URIs specified by m.ServiceProvider.MetadataURL and
// m.ServiceProvider.AcsURL.
func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metadataURL, _ := url.Parse(m.ServiceProvider.MetadataURL)
	if r.URL.Path == metadataURL.Path {
		buf, _ := xml.MarshalIndent(m.ServiceProvider.Metadata(), "", "  ")
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		w.Write(buf)
		return
	}

	acsURL, _ := url.Parse(m.ServiceProvider.AcsURL)
	if r.URL.Path == acsURL.Path {
		r.ParseForm()
		assertion, err := m.ServiceProvider.ParseResponse(r, m.getPossibleRequestIDs(r))
		if err != nil {
			if parseErr, ok := err.(*saml.InvalidResponseError); ok {
				log.Printf("RESPONSE: ===\n%s\n===\nNOW: %s\nERROR: %s",
					parseErr.Response, parseErr.Now, parseErr.PrivateErr)
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		m.Authorize(w, r, assertion)
		return
	}

	http.NotFoundHandler().ServeHTTP(w, r)
}

// RequireAccount is HTTP middleware that requires that each request be
// associated with a valid session. If the request is not associated with a valid
// session, then rather than serve the request, the middlware redirects the user
// to start the SAML auth flow.
func (m *Middleware) RequireAccount(handler http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if m.IsAuthorized(r) {
			handler.ServeHTTP(w, r)
			return
		}

		// If we try to redirect when the original request is the ACS URL we'll
		// end up in a loop. This is a programming error, so we panic here. In
		// general this means a 500 to the user, which is preferable to a
		// redirect loop.
		acsURL, _ := url.Parse(m.ServiceProvider.AcsURL)
		if r.URL.Path == acsURL.Path {
			panic("don't wrap Middleware with RequireAccount")
		}

		req, err := m.ServiceProvider.MakeAuthenticationRequest(
			m.ServiceProvider.GetSSOBindingLocation(saml.HTTPRedirectBinding))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// relayState is limited to 80 bytes but also must be integrety protected.
		// this means that we cannot use a JWT because it is way to long. Instead
		// we set a cookie that corresponds to the state
		relayState := base64.URLEncoding.EncodeToString(randomBytes(42))

		state := jwt.New(jwt.GetSigningMethod("RS256"))
		claims := state.Claims.(jwt.MapClaims)
		claims["id"] = req.ID
		claims["uri"] = r.URL.String()
		signedState, err := state.SignedString(m.ServiceProvider.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     fmt.Sprintf("saml_%s", relayState),
			Value:    signedState,
			MaxAge:   int(saml.MaxIssueDelay.Seconds()),
			HttpOnly: false,
			Path:     acsURL.Path,
		})
		redirectURL, _ := req.Redirect(relayState)

		w.Header().Add("Location", redirectURL.String())
		w.WriteHeader(http.StatusFound)
		return
	}
	return http.HandlerFunc(fn)
}

func (m *Middleware) getPossibleRequestIDs(r *http.Request) []string {
	rv := []string{}
	for _, cookie := range r.Cookies() {
		if !strings.HasPrefix(cookie.Name, "saml_") || cookie.Value == "" {
			continue
		}
		token, err := jwt.Parse(cookie.Value, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("Unexpected signing method: %v", t.Header["alg"])
			}

			return m.ServiceProvider.Key.Public(), nil
		})
		if err != nil || !token.Valid {
			log.Printf("... invalid token %s", err)
			continue
		}
		claims := token.Claims.(jwt.MapClaims)
		rv = append(rv, claims["id"].(string))
	}

	// If IDP initiated requests are allowed, then we can expect an empty response ID.
	if m.AllowIDPInitiated {
		rv = append(rv, "")
	}

	return rv
}

// Authorize is invoked by ServeHTTP when we have a new, valid SAML assertion.
// It sets a cookie that contains a signed JWT containing the assertion attributes.
// It then redirects the user's browser to the original URL contained in RelayState.
func (m *Middleware) Authorize(w http.ResponseWriter, r *http.Request, assertion *saml.Assertion) {
	redirectURI := "/"
	if r.Form.Get("RelayState") != "" {
		stateCookie, err := r.Cookie(fmt.Sprintf("saml_%s", r.Form.Get("RelayState")))
		if err != nil {
			log.Printf("cannot find corresponding cookie: %s", fmt.Sprintf("saml_%s", r.Form.Get("RelayState")))
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		state, err := jwt.Parse(stateCookie.Value, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("Unexpected signing method: %v", t.Header["alg"])
			}

			return m.ServiceProvider.Key.Public(), nil
		})
		if err != nil || !state.Valid {
			log.Printf("Cannot decode state JWT: %s (%s)", err, stateCookie.Value)
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		claims := state.Claims.(jwt.MapClaims)
		redirectURI = claims["uri"].(string)

		// delete the cookie
		stateCookie.Value = ""
		stateCookie.Expires = time.Time{}
		http.SetCookie(w, stateCookie)
	}

	token := jwt.New(jwt.GetSigningMethod("RS256"))
	claims := token.Claims.(jwt.MapClaims)
	for _, attr := range assertion.AttributeStatement.Attributes {
		valueStrings := []string{}
		for _, v := range attr.Values {
			valueStrings = append(valueStrings, v.Value)
		}
		claimName := attr.FriendlyName
		if claimName == "" {
			claimName = attr.Name
		}
		claims[claimName] = valueStrings
	}
	claims["exp"] = saml.TimeNow().Add(cookieMaxAge).Unix()
	signedToken, err := token.SignedString(m.ServiceProvider.Key)
	if err != nil {
		panic(err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    signedToken,
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: false,
		Path:     "/",
	})

	http.Redirect(w, r, redirectURI, http.StatusFound)
}

// IsAuthorized is invoked by RequireAccount to determine if the request
// is already authorized or if the user's browser should be redirected to the
// SAML login flow. If the request is authorized, then the request headers
// starting with X-Saml- for each SAML assertion attribute are set. For example,
// if an attribute "uid" has the value "alice@example.com", then the following
// header would be added to the request:
//
//     X-Saml-Uid: alice@example.com
//
// It is an error for this function to be invoked with a request containing
// any headers starting with X-Saml. This function will panic if you do.
func (m *Middleware) IsAuthorized(r *http.Request) bool {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	token, err := jwt.Parse(cookie.Value, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", t.Header["alg"])
		}

		return m.ServiceProvider.Key.Public(), nil
	})
	if err != nil || !token.Valid {
		return false
	}

	// It is an error for the request to include any X-SAML* headers,
	// because those might be confused with ours. If we encounter any
	// such headers, we abort the request, so there is no confustion.
	for headerName := range r.Header {
		if strings.HasPrefix(headerName, "X-Saml") {
			panic("X-Saml-* headers should not exist when this function is called")
		}
	}

	claims := token.Claims.(jwt.MapClaims)
	for claimName, claimValue := range claims {
		if claimName == "exp" {
			continue
		}
		for _, claimValueStr := range claimValue.([]interface{}) {
			r.Header.Add(fmt.Sprintf("X-Saml-%s", claimName), claimValueStr.(string))
		}
	}
	return true
}

// RequireAttribute returns a middleware function that requires that the
// SAML attribute `name` be set to `value`. This can be used to require
// that a remote user be a member of a group. It relies on the X-Saml-* headers
// that RequireAccount adds to the request.
//
// For example:
//
//     goji.Use(m.RequireAccount)
//     goji.Use(RequireAttributeMiddleware("eduPersonAffiliation", "Staff"))
//
func RequireAttribute(name, value string) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			if values, ok := r.Header[http.CanonicalHeaderKey(fmt.Sprintf("X-Saml-%s", name))]; ok {
				for _, actualValue := range values {
					if actualValue == value {
						handler.ServeHTTP(w, r)
						return
					}
				}
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		}
		return http.HandlerFunc(fn)
	}
}
