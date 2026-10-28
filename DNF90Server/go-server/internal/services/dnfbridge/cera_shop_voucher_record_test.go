package dnfbridge

import "testing"

// 商城记录的"券实际扣了多少"契约（2026-10-05）。
//
// ── 为什么要专门锁住它 ────────────────────────────────────────────────────
// 现场排查一笔"疑似白拿"的订单时卡住了：那份记录里只有
//
//	实扣点券 = 0
//	欢乐代币券余额 = 0        ← "之后"的余额，不是"扣了多少"
//
// 两个 0 摆在一起，看起来就是一分钱没付。实际那一单是**全额用欢乐代币券支付**的
// （14000 券），只是记录里根本没写"券扣了多少"。为了定案只能把服务器上 3GB 的
// 加密封包日志拉回来解密，再去 packet_log 里翻 paid_happy_token_cera。
//
// 同一条记录里 gold_* 早就有 before/after/spent 三件套，券这一路是**漏项**。
// 这组测试钉住三件事：
//  1. 全额券付也必须能在记录里一眼看出"付了、付了多少"；
//  2. 点券+券混合支付时两边都要如实记录，且 before = after + spent 恒成立；
//  3. 纯点券支付**不能**凭空多出券字段（既有记录的字段集不被污染）。
func TestRecordCeraShopPurchaseLogsVoucherColumns(t *testing.T) {
	const (
		wantHappySpent = 14000
		wantCeraPaid   = 1000
	)

	cases := []struct {
		name string
		// 入参
		paidCera   int64
		paidToken  int64
		paidHappy  int64
		ceraAfter  int64
		tokenAfter int64
		happyAfter int64
		// 期望出现在记录里的字段（键 → 值）；未列出的券字段必须**缺席**
		want map[string]float64
		// 必须缺席的字段
		absent []string
	}{
		{
			// ★ 还原现场那一单：实扣点券 0，但实际付了 14000 欢乐代币券。
			// 改造前这条记录只有 cera_spent=0 和 happy_token_cera_after=0，无法自证。
			name:       "全额欢乐代币券支付：必须看得出付了 14000",
			paidCera:   0,
			paidHappy:  wantHappySpent,
			ceraAfter:  1011, // 点券一分未动
			happyAfter: 0,    // 券被扣光
			want: map[string]float64{
				"cera_spent":              0,
				"happy_token_cera_spent":  wantHappySpent,
				"happy_token_cera_before": wantHappySpent, // 0 + 14000
				"happy_token_cera_after":  0,
				"token_cera_after":        0,
			},
			absent: []string{"token_cera_spent", "token_cera_before"},
		},
		{
			// 混合支付：点券 + 欢乐代币券共同承担。两边都要记，且 before 可核算。
			name:       "点券+欢乐代币券混合支付",
			paidCera:   wantCeraPaid,
			paidHappy:  13000,
			ceraAfter:  1011,
			happyAfter: 0,
			want: map[string]float64{
				"cera_spent":              wantCeraPaid,
				"cera_before":             1011 + wantCeraPaid,
				"happy_token_cera_spent":  13000,
				"happy_token_cera_before": 13000,
			},
		},
		{
			name:       "代币券支付（另一种券）",
			paidToken:  900,
			ceraAfter:  500,
			tokenAfter: 100,
			want: map[string]float64{
				"token_cera_spent":  900,
				"token_cera_before": 1000, // 100 + 900
				"token_cera_after":  100,
			},
			absent: []string{"happy_token_cera_spent", "happy_token_cera_before"},
		},
		{
			// 纯点券支付：既有 177 行商品的记录字段集不该被撑大。
			name:      "纯点券支付不写券字段",
			paidCera:  80,
			ceraAfter: 120,
			want: map[string]float64{
				"cera_spent":  80,
				"cera_before": 200,
			},
			absent: []string{
				"token_cera_spent", "token_cera_before",
				"happy_token_cera_spent", "happy_token_cera_before",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, _ := newTestRecorder(t)
			service := &Service{}
			service.records.Store(recorder)
			session := &gameSession{accountID: "dnf:5"}
			session.selectedCharacterID = 22

			service.recordCurrentCeraShopPurchase(
				session,
				currentCeraShopPurchaseRequest{
					PaymentMode: currentCeraShopPaymentModeCera,
					Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 102929, AttributeValue: 255}},
				},
				currentCeraShopCommitResult{
					Products:           []currentCeraShopProduct{{CommodityID: 102929, ItemID: 490700963, Count: 60, CeraPrice: 14000, Section: "item"}},
					CeraAfter:          tc.ceraAfter,
					PaidCera:           tc.paidCera,
					TokenCera:          tc.tokenAfter,
					PaidTokenCera:      tc.paidToken,
					HappyTokenCera:     tc.happyAfter,
					PaidHappyTokenCera: tc.paidHappy,
				},
				true, "", 19,
			)

			records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindCera})
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%d err=%v", len(records), err)
			}
			record := records[0]

			for key, want := range tc.want {
				got, present := record[key]
				if !present {
					t.Fatalf("记录缺少 %q（这就是「看不出付没付」的根因）：%+v", key, record)
				}
				if got != want {
					t.Fatalf("%s = %v，期望 %v（完整记录 %+v）", key, got, want, record)
				}
			}
			for _, key := range tc.absent {
				if _, present := record[key]; present {
					t.Fatalf("不该出现的字段 %q 出现了：%+v", key, record)
				}
			}
		})
	}
}
