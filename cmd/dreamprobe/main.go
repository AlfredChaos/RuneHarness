package main

import (
	"context"
	"time"
	"fmt"
	"os"

	"runeharness/internal/llm"
	"runeharness/internal/memory"
	"runeharness/internal/scope"
	"runeharness/internal/session/sqlite"
)

func main() {
	store, err := sqlite.Open("/tmp/sess_probe.db")
	if err != nil { fmt.Println("sess open:", err); os.Exit(1) }
	defer store.Close()
	model := llm.NewClient(os.Getenv("OPENAI_API_KEY"), os.Getenv("OPENAI_BASE_URL"), os.Getenv("OPENAI_MODEL"))
	mem, err := memory.New(store, model, memory.Config{
		DBPath:  "/tmp/mem_probe.db",
		Profile: memory.Lookup("companion"),
		Dream:   true,
	})
	if err != nil { fmt.Println("memory.New:", err); os.Exit(1) }
	defer mem.Close()
	sc := scope.SystemScope("/tmp")
	ctx, cancel := context.WithTimeout(scope.WithScope(context.Background(), sc), 8*time.Minute)
	defer cancel()
	st, err := mem.Dream(ctx, true)
	fmt.Printf("stats=%+v err=%v\n", st, err)
}
