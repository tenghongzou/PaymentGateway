package gateway

// must 讓測試以單一運算式取得 (value, error) 呼叫的結果；錯誤時 panic 使測試立即失敗。
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
