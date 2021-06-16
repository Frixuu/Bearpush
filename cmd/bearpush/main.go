package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"github.com/frixuu/bearpush"
	"github.com/frixuu/bearpush/config/templates"
	"github.com/frixuu/bearpush/internal/util"
	"github.com/frixuu/bearpush/server"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
)

func main() {

	logger := CreateLogger()
	zap.ReplaceGlobals(logger.Desugar())
	defer logger.Sync()

	app := &cli.App{
		EnableBashCompletion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config-dir",
				Aliases: []string{"c"},
				Usage:   "Path to a directory containing configuration of this app.",
				Value:   bearpush.DefaultConfigDir,
			},
		},
		Commands: []*cli.Command{
			{
				Name:    "product",
				Aliases: []string{"p"},
				Usage:   "Options for product manipulation.",
				Subcommands: []*cli.Command{
					{
						Name:  "new",
						Usage: "Creates a new product template.",
						Action: func(c *cli.Context) error {

							productName := c.Args().First()
							if productName == "" {
								fmt.Println("Product name not specified!")
							}

							config, err := bearpush.LoadConfig(c.String("config-dir"))
							if err != nil {
								fmt.Println("Cannot load config")
								return err
							}

							dir := filepath.Join(config.Path, "products")
							err = os.MkdirAll(dir, 0740)
							if err != nil && !os.IsExist(err) {
								return err
							}

							productPath := filepath.Join(dir, productName+".yml")
							_, err = os.Stat(productPath)
							if err == nil || !os.IsNotExist(err) {
								fmt.Printf("A product named %s already exists.\n", productName)
								return os.ErrExist
							}

							file, err := os.Create(productPath)
							defer file.Close()
							if err != nil {
								fmt.Printf("Cannot open file %s for writing\n", productPath)
								return err
							}

							_, err = file.WriteString(templates.GenerateProductFile(productName))
							if err != nil {
								fmt.Printf("An error occurred while writing to file %s\n", productPath)
								return err
							}

							fmt.Printf("Configuration for new product %s scaffolded successfully.\n", productName)
							fmt.Printf("It has been saved in %s.\n", productPath)
							return nil
						},
					},
				},
			},
		},
		Action: func(c *cli.Context) error {

			config, err := bearpush.LoadConfig(c.String("config-dir"))
			if err != nil {
				logger.Error("Cannot load config")
				return err
			}

			logger.Info("Config directory: %s", config.Path)
			appContext, err := bearpush.ContextFromConfig(config)
			if err != nil {
				logger.Errorf("Cannot create app context: %s\n", err)
				return err
			}

			for name, p := range appContext.Products {
				logger.Infof("Loaded product %s, token strategy %v", name, p.TokenSettings.Strategy)
			}

			gin.SetMode(gin.ReleaseMode)
			gin.DefaultWriter = io.Discard
			gin.DefaultErrorWriter = io.Discard

			router := gin.Default()
			router.MaxMultipartMemory = 8 << 20 // 8 MiB

			// Log all requests with Zap
			router.Use(ginzap.Ginzap(logger.Desugar(), time.RFC3339, false))
			// Log all panics with stacktraces
			router.Use(ginzap.RecoveryWithZap(logger.Desugar(), true))

			router.GET("/ping", func(c *gin.Context) {
				c.JSON(200, gin.H{
					"message": "pong",
				})
			})

			v1 := router.Group("/v1")
			{
				v1.POST("/upload/:product", server.ValidateToken(appContext), func(c *gin.Context) {

					product := c.Param("product")
					p, ok := appContext.Products[product]
					if !ok {
						c.JSON(http.StatusBadRequest, gin.H{
							"error":   4,
							"message": "Resource does not exist.",
						})
						return
					}

					file, err := c.FormFile("artifact")
					if err != nil {
						logger.Warn(err)
						c.String(http.StatusBadRequest, fmt.Sprintf("Error while uploading: %s", err))
						return
					}

					tempDir, err := ioutil.TempDir("", "bearpush-")
					if err != nil {
						logger.Error(err)
						c.String(http.StatusInternalServerError,
							"Could not create a temporary directory for artifact. Check logs for details.")
						return
					}
					defer util.TryRemoveDir(tempDir)

					artifactPath := path.Join(tempDir, "artifact")
					err = c.SaveUploadedFile(file, artifactPath)
					if err != nil {
						logger.Error("Cannot save artifact: %s", err)
						c.String(http.StatusInternalServerError,
							"Could not save the uploaded artifact. Check logs for details.")
						return
					}

					if p.Script != "" {
						cmd := exec.Command(p.Script)
						cmd.Env = append(os.Environ(),
							fmt.Sprintf("ARTIFACT_PATH=%s", artifactPath),
						)

						stdoutPipe, err := cmd.StdoutPipe()
						if err != nil {
							logger.Errorf("Cannot grab stdout pipe: %s\n", err)
						}

						stderrPipe, err := cmd.StderrPipe()
						if err != nil {
							logger.Errorf("Cannot grab stderr pipe: %s\n", err)
						}

						if err := cmd.Start(); err != nil {
							logger.Errorf("Cannot start: %s\n", err)
						}

						_, err = io.ReadAll(stdoutPipe)
						if err != nil {
							logger.Errorf("Cannot read stdout: %s\n", err)
						}

						_, err = io.ReadAll(stderrPipe)
						if err != nil {
							logger.Errorf("Cannot read stderr: %s\n", err)
						}

						if err := cmd.Wait(); err != nil {
							logger.Errorf("Command failed: %s\n", err)
							c.JSON(http.StatusUnprocessableEntity, gin.H{
								"error":   8,
								"message": "Pipeline associated with resource errored.",
							})
							return
						}
					}

					c.String(http.StatusOK,
						fmt.Sprintf("Artifact for product %s processed successfully.", product))
				})
			}

			port := server.DeterminePort()
			logger.Info("The server will bind to ", port)

			srv := &http.Server{
				Addr:    port,
				Handler: router,
			}

			// Listen in a goroutine
			go server.Start(srv, logger.Desugar())

			util.WaitForInterrupt()
			logger.Info("Shutting down the server")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			if err := srv.Shutdown(ctx); err != nil {
				logger.Fatal("Server forced to shutdown: ", err)
			}

			return nil
		},
	}

	app.Setup()
	err := app.Run(os.Args)
	if err != nil {
		logger.Fatal(err)
	}
}