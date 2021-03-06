package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/fourcube/goiban"
	m "github.com/fourcube/goiban-service/metrics"
	_ "github.com/go-sql-driver/mysql"
	"github.com/julienschmidt/httprouter"
	"github.com/pmylund/go-cache"
	"github.com/rs/cors"
)

/**
Handles requests and serves static pages.

route							description
--------------------			--------------------------------------------------------
/validate/{iban} 				Tries to validate {iban} and returns a HTTP response
								in JSON. See goiban.ValidationResult for details of the
								data returned.

/*								Renders static content from the "./static" folder
*/
var (
	c            = cache.New(5*time.Minute, 30*time.Second)
	db           *sql.DB
	err          error
	PREP_ERR     error
	ENV          string
	metrics      *m.KeenMetrics
	inmemMetrics = m.NewInmemMetricsRegister()
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: goiban-service <port> <dburl> [<env>] [keenProjectID] [keenWriteAPIKey]")
		return
	}

	port := os.Args[1]
	mysqlURL := os.Args[2]

	if len(os.Args) < 6 {
		ENV = "Test"
	} else {
		ENV = os.Args[3]
		metrics = &m.KeenMetrics{
			ProjectID:   os.Args[4],
			WriteAPIKey: os.Args[5],
		}
	}

	listen(port, ENV, mysqlURL)
}

func listen(port string, environment string, dbUrl string) {
	log.Printf("Setting env to %v", environment)

	db, err = sql.Open("mysql", dbUrl)

	if err != nil {
		log.Fatalf("Error opening DB connection: %v", err)
	}

	router := httprouter.New()
	corsHandler := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET"},
	})

	router.GET("/validate/:iban", validationHandler)
	router.GET("/countries", countryCodeHandler)
	router.GET("/calculate/:countryCode/:bankCode/:accountNumber", calculateIBAN)
	router.GET("/v2/calculate/:countryCode/:bankCode/:accountNumber", calculateAndValidateIBAN)
	router.Handler("GET", "/metrics", http.Handler(inmemMetrics))

	//Only host the static template when the ENV is 'Live' or 'Test'
	if environment == "Live" || environment == "Test" {
		router.NotFound = http.FileServer(http.Dir("static"))
	}

	handler := corsHandler.Handler(router)
	err = http.ListenAndServe(":"+port, handler)

	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

// Processes requests to the /validate/ url
func validationHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	var strRes string
	config := map[string]bool{}
	// Set response type to application/json.
	// See: https://www.owasp.org/index.php/XSS_(Cross_Site_Scripting)_Prevention_Cheat_Sheet#RULE_.233.1_-_HTML_escape_JSON_values_in_an_HTML_context_and_read_the_data_with_JSON.parse
	w.Header().Add("Content-Type", "application/json; charset=utf-8")
	// Allow CORS
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// extract iban parameter
	iban := ps.ByName("iban")

	// check for additional request parameters
	validateBankCodeQueryParam := r.FormValue("validateBankCode")
	config["validateBankCode"] = toBoolean(validateBankCodeQueryParam)

	// check for additional request parameters
	getBicQueryParam := r.FormValue("getBIC")
	config["getBIC"] = toBoolean(getBicQueryParam)

	// hit the cache
	value, found := hitCache(iban + strconv.FormatBool(config["getBIC"]) + strconv.FormatBool(config["validateBankCode"]))
	if found {
		go logFromCacheEntry(ENV, value)
		fmt.Fprintf(w, value)
		return
	}

	// no value for request parameter
	// return HTTP 400
	if len(iban) == 0 {
		res, _ := json.MarshalIndent(goiban.NewValidationResult(false, "Empty request.", iban), "", "  ")
		strRes = string(res)
		w.Header().Add("Content-Length", strconv.Itoa(len(strRes)))
		// put to cache and render
		// c.Set(iban, strRes, 0)
		http.Error(w, strRes, http.StatusBadRequest)
		return
	}

	// IBAN is not parseable
	// return HTTP 200
	parserResult := goiban.IsParseable(iban)

	if !parserResult.Valid {
		res, _ := json.MarshalIndent(goiban.NewValidationResult(false, "Cannot parse as IBAN: "+parserResult.Message, iban), "", "  ")
		strRes = string(res)
		w.Header().Add("Content-Length", strconv.Itoa(len(strRes)))

		// put to cache and render
		c.Set(iban+strconv.FormatBool(config["getBIC"])+strconv.FormatBool(config["validateBankCode"]), strRes, 0)
		fmt.Fprintf(w, strRes)
		return
	}

	// Try to validate
	parsedIban := goiban.ParseToIban(iban)
	result := parsedIban.Validate()

	// intermediate result
	if len(config) > 0 {
		result = additionalData(parsedIban, result, config)
	}

	res, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Println(err)
	}

	strRes = string(res)
	w.Header().Add("Content-Length", strconv.Itoa(len(strRes)))
	// put to cache and render

	go logFromIbanResult(ENV, parsedIban)

	key := iban + strconv.FormatBool(config["getBIC"]) + strconv.FormatBool(config["validateBankCode"])

	c.Set(key, strRes, 0)
	fmt.Fprintf(w, strRes)
	return
}

func toBoolean(value string) bool {
	switch value {
	case "1":
		return true
	case "true":
		return true
	default:
		return false
	}
}

func additionalData(iban *goiban.Iban, intermediateResult *goiban.ValidationResult, config map[string]bool) *goiban.ValidationResult {
	validateBankCode, ok := config["validateBankCode"]
	if ok && validateBankCode {
		intermediateResult = goiban.ValidateBankCode(iban, intermediateResult, db)
	}

	getBic, ok := config["getBIC"]
	if ok && getBic {
		intermediateResult = goiban.GetBic(iban, intermediateResult, db)
	}
	return intermediateResult
}

func hitCache(iban string) (string, bool) {
	val, ok := c.Get(iban)
	if ok {
		return val.(string), ok
	}

	return "", false

}

// Only logs when metrics is defined
func logFromCacheEntry(ENV string, value string) {
	if metrics != nil {
		metrics.LogRequestFromValidationResult(ENV, value)
	} else {
		var result *goiban.ValidationResult
		json.Unmarshal([]byte(value), &result)

		inmemMetrics.Register(m.ValidationResultToEvent(result))
	}
}

// Only logs when metrics is defined
func logFromIbanResult(ENV string, value *goiban.Iban) {
	if metrics != nil {
		metrics.WriteLogRequest(ENV, value)
	} else {
		inmemMetrics.Register(m.IbanToEvent(value))
	}
}
