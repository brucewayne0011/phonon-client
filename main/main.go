package main

import (
	"github.com/GridPlus/phonon-client/card"
	log "github.com/sirupsen/logrus"
)

func main() {

	//Deposit Phonon routine
	// onePhonon := make(map[int]int)
	// onePhonon[1] = 1

	// s, _ := phonon.NewSession()

	// s.Deposit(0, 0, onePhonon)

	// s.ListPhonons(10)

	// sc := card.Safecard{}
	// // err := sc.Connect()
	// // if err != nil {
	// // 	log.Error("unable to connect to smartcard. err: ", err)
	// // }
	// sc.Connect()
	sc, err := card.Connect()
	if err != nil {
		log.Error("error connecting to card. err: ", err)
	}
	log.Infof("safecard: %+v", sc)

	err = sc.Select()
	if err != nil {
		log.Error("error selecting applet. err: ", err)
	}

	err = sc.Pair()

	// sc.Pair()
	// sc.OpenSecureChannel()

	// sc.Select()

	// seed, err := sc.ExportSeed()
	// if err != nil {
	// 	log.Error("unable to export seed. err: ", err)
	// }
	// fmt.Println("seed: ", seed)

}
