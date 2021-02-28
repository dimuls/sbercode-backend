package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/doug-martin/goqu/v9"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	_ "github.com/lib/pq"

	"github.com/dimuls/sberhack-backend/core"
)

const (
	xAuthToken = "X-Auth-Token"
	xSubjToken = "X-Subject-Token"
)

var migrations = []string{
	`create table if not exists dashboard (id bigserial primary key, user_id text, name text, graphs jsonb, unique(user_id, name))`,
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		_, err := db.Exec(m)
		if err != nil {
			return err
		}
	}
	return nil
}

const iamAPI = "https://iam.ru-moscow-1.hc.sbercloud.ru/v3"
const cesAPI = "https://ces.ru-moscow-1.hc.sbercloud.ru/V1.0"

type TokenResp struct {
	Token struct {
		User struct {
			ID string
		}
	}
}

type Dashboard struct {
	ID     int             `db:"id" json:"id"`
	Name   string          `db:"name" json:"name"`
	Graphs json.RawMessage `db:"graphs" json:"graphs"`
}

type DashboardsRes struct {
	Dashboards []Dashboard `json:"dashboard"`
}

type DashboardRes struct {
	Dashboard Dashboard `json:"dashboard"`
}

type AddDashboardsRes struct {
	ID int `json:"id"`
}

func main() {

	s := core.Signer{
		Key:    os.Getenv("SIGNER_KEY"),
		Secret: os.Getenv("SIGNER_SECRET"),
	}

	rawDB, err := sql.Open("postgres", os.Getenv("PG_URI"))
	if err != nil {
		log.Fatal("failed to open db:", err)
	}

	err = migrate(rawDB)
	if err != nil {
		log.Fatal("failed to migrate db:", err)
	}

	db := goqu.New("postgres", rawDB)

	app := fiber.New(fiber.Config{
		ReadTimeout: 10 * time.Second,
	})

	app.Use(recover.New(), logger.New(logger.Config{
		Next: func(c *fiber.Ctx) bool {
			return string(c.Request().URI().Path()) == "/health-check"
		},
	}))

	app.Get("/health-check", func(c *fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	r := app.Group("/", func(c *fiber.Ctx) error {

		token := string(c.Request().Header.Peek(xAuthToken))

		if token == "" {
			return c.Status(http.StatusForbidden).SendString("token is absent")
		}

		url := iamAPI + "/auth/tokens"

		r, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return c.Status(http.StatusInternalServerError).SendString(
				"failed to create http request request: " + err.Error())
		}

		r.Header.Set(xAuthToken, token)
		r.Header.Set(xSubjToken, token)
		r.Header.Set("Content-Type", "application/json")

		res, err := http.DefaultClient.Do(r)
		if err != nil {
			return c.Status(http.StatusInternalServerError).SendString(
				"failed to do http request: " + err.Error())
		}

		defer res.Body.Close()

		if res.StatusCode >= 500 {
			return c.Status(http.StatusInternalServerError).
				SendString("unable to check token")
		}

		if res.StatusCode != http.StatusOK {
			return c.Status(http.StatusUnauthorized).
				SendString("invalid token")
		}

		var tokenRes TokenResp

		err = json.NewDecoder(res.Body).Decode(&tokenRes)
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to unmarshal token check response")
		}

		c.Locals("userID", tokenRes.Token.User.ID)

		return c.Next()
	})

	r.Get("/ces/*", func(c *fiber.Ctx) error {

		url := cesAPI

		path := c.Params("*")
		if path != "" {
			url += "/" + path
		}

		query := string(c.Request().URI().QueryString())
		if query != "" {
			url += "?" + query
		}

		log.Printf("[ces request] url=%s\n", url)

		r, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to create http request: " + err.Error())
		}

		r.Header.Add("x-stage", "RELEASE")
		s.Sign(r)

		res, err := http.DefaultClient.Do(r)
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to do http request: " + err.Error())
		}

		defer res.Body.Close()

		return c.Status(res.StatusCode).SendStream(res.Body)
	})

	r.Get("/dashboards", func(c *fiber.Ctx) error {
		userID, ok := c.Locals("userID").(string)
		if !ok {
			return c.Status(http.StatusInternalServerError).
				SendString("expected local userID string")
		}

		var ds []Dashboard

		db.Select("id", "name", "graphs").From("dashboard").
			Where(goqu.Ex{"user_id": userID}).Executor().ScanStructs(&ds)

		return c.JSON(DashboardsRes{Dashboards: ds})
	})

	r.Get("/dashboards/:id", func(c *fiber.Ctx) error {
		userID, ok := c.Locals("userID").(string)
		if !ok {
			return c.Status(http.StatusInternalServerError).
				SendString("expected local userID string")
		}

		dashboardID, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(http.StatusBadRequest).
				SendString("failed to parse dashboard ID")
		}

		var d Dashboard

		found, err := db.Select("id", "user_id", "name", "graphs").
			From("dashboard").
			Where(goqu.Ex{"id": dashboardID, "user_id": userID}).
			Executor().ScanStruct(&d)
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to get dashboard from DB: " + err.Error())
		}
		if !found {
			return c.SendStatus(http.StatusNotFound)
		}

		return c.JSON(DashboardRes{Dashboard: d})
	})

	r.Delete("/dashboards/:id", func(c *fiber.Ctx) error {
		userID, ok := c.Locals("userID").(string)
		if !ok {
			return c.Status(http.StatusInternalServerError).
				SendString("expected local userID string")
		}

		dashboardID, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(http.StatusBadRequest).
				SendString("failed to parse dashboard ID")
		}

		_, err = db.From("dashboard").Delete().Where(
			goqu.Ex{"id": dashboardID, "user_id": userID}).Executor().Exec()
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to delete dashboard from db: " + err.Error())
		}

		return c.SendStatus(http.StatusOK)
	})

	r.Post("/dashboards", func(c *fiber.Ctx) error {

		userID, ok := c.Locals("userID").(string)
		if !ok {
			return c.Status(http.StatusInternalServerError).
				SendString("expected local userID string")
		}

		var d Dashboard

		err := json.Unmarshal(c.Body(), &d)
		if err != nil {
			return c.Status(http.StatusBadRequest).SendString(
				"failed to JSON unmarshal dashboard: " + err.Error())
		}

		var id int

		_, err = db.Insert("dashboard").Cols("user_id", "name", "graphs").
			Vals(goqu.Vals{userID, d.Name, goqu.L("?::jsonb", string(d.Graphs))}).
			Returning("id").Executor().ScanVal(&id)
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to insert dashboard to db:" + err.Error())
		}

		return c.JSON(AddDashboardsRes{ID: id})
	})

	r.Put("/dashboards", func(c *fiber.Ctx) error {

		userID, ok := c.Locals("userID").(string)
		if !ok {
			return c.Status(http.StatusInternalServerError).
				SendString("expected local userID string")
		}

		var d Dashboard

		err := json.Unmarshal(c.Body(), &d)
		if err != nil {
			return c.Status(http.StatusBadRequest).SendString(
				"failed to JSON unmarshal dashboard: " + err.Error())
		}

		_, err = db.Update("dashboard").Set(goqu.Record{
			"name":   d.Name,
			"graphs": goqu.L("?::jsonb", string(d.Graphs)),
		}).Where(goqu.Ex{"id": d.ID, "user_id": userID}).Executor().Exec()
		if err != nil {
			return c.Status(http.StatusInternalServerError).
				SendString("failed to update dashboard in db:" + err.Error())
		}

		return c.SendStatus(http.StatusOK)
	})

	go app.Listen("0.0.0.0:80")

	signals := make(chan os.Signal)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	<-signals
}
