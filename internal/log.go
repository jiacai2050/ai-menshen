package aimenshen

import "log"

func logInfo(format string, v ...any) {
	log.Printf("[INFO] "+format, v...)
}

func logError(format string, v ...any) {
	log.Printf("[ERROR] "+format, v...)
}
