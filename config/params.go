package config

import (
	"log"

	"github.com/caarlos0/env"
)

type Params struct {
	ID                string  `env:"ID"`
	Tagg              int     `env:"T_AGG"                 envDefault:"1"`
	Rmax              int     `env:"R_MAX"                 envDefault:"3"`
	RespProbability   float64 `env:"RESP_PROBABILITY"      envDefault:"0.7"`
	EpochLength       int     `env:"EPOCH_LENGTH"          envDefault:"10"`
	FirstRoundWaitSec int     `env:"FIRST_ROUND_WAIT_SEC"  envDefault:"5"`
}

func LoadParamsFromEnv() Params {
	var p Params
	if err := env.Parse(&p); err != nil {
		log.Fatalln(err)
	}
	return p
}
