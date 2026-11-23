package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-vgo/robotgo"
)

// CursorWatcher представляет состояние отслеживания курсора
type CursorWatcher struct {
	isRunning   atomic.Bool
	checkMillis int
	targetX     int
	targetY     int
}

// NewCursorWatcher создает новый экземпляр CursorWatcher
func NewCursorWatcher(checkMillis int, startX, startY int) *CursorWatcher {
	return &CursorWatcher{
		checkMillis: checkMillis,
		targetX:     startX,
		targetY:     startY,
	}
}

// IsRunning сообщает, запущено ли наблюдение
func (cw *CursorWatcher) IsRunning() bool {
	return cw.isRunning.Load()
}

// Start запускает отслеживание курсора
func (cw *CursorWatcher) Start() {
	if !cw.isRunning.CompareAndSwap(false, true) {
		fmt.Println("Курсор уже отслеживается!")
		return
	}

	fmt.Println("🔥 Запущено отслеживание курсора!")

	go func() {
		// Устанавливаем курсор в начальную позицию
		robotgo.MoveMouse(cw.targetX, cw.targetY)
		fmt.Printf("Координаты курсора установлены: (%d, %d)\n", cw.targetX, cw.targetY)

		checkInterval := time.Duration(cw.checkMillis) * time.Millisecond

		for cw.isRunning.Load() {
			currentX, currentY := robotgo.GetMousePos()
			if currentX != cw.targetX || currentY != cw.targetY {
				fmt.Printf("🚨 Курсор сдвинулся! Новая позиция: (%d, %d)\n", currentX, currentY)
				cw.targetX, cw.targetY = currentX, currentY // Обновляем целевую позицию
				notify(fmt.Sprintf("Курсор сдвинулся! Новая позиция: (%d, %d)", currentX, currentY))
			}
			time.Sleep(checkInterval)
		}
	}()
}

// Stop останавливает отслеживание курсора
func (cw *CursorWatcher) Stop() {
	if !cw.isRunning.CompareAndSwap(true, false) {
		fmt.Println("Курсор не отслеживается!")
		return
	}

	fmt.Println("❌ Отслеживание курсора остановлено!")
}
