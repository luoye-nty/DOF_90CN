-- ============================================================
--  DNF90 CDK 批量生成（设置数量即可；奖励/名称可改）
--
--  用法：
--    1) 改下面 @n 的数字（要生成几张），其它参数按需改
--    2) 在服务器上执行：
--         mysql -udnf90 -p"密码" dnf_local < gen_cdk_codes.sql
--       或先进入 mysql（mysql -udnf90 -p dnf_local），再把整段粘进去执行
--
--  说明：
--    · 码格式 XXXX-XXXX-XXXX-XXXX-XXXX（5 组 × 4 位 = 20 位有效字符），字符集
--      去掉容易看错的 0 O 1 I，与登录器 / 服务端 NormalizeRedeemCode 的规范化结果一致
--    · 每张码独立随机；靠唯一键 + INSERT IGNORE 去重，可反复执行，只增不改
--    · 谁用谁得：点券进"所选角色所属账号"的通用余额，物品发到所选角色的系统邮件，
--      黑钻是账号级权益。CDK 没有"指定发给某个账号"的能力
--    · @maxuse = 0 → 这张码不限总兑换次数（不限人数）；但同一账号对同一个码只能兑
--      一次（redeem_redemptions 上 (account_id, redeem_code) 唯一键决定）
--      @maxuse = 1 → 谁先兑谁得；@maxuse = N → 限量 N 次
--    · 新生成的码一律 enabled = 1、redeemed_count = 0
-- ============================================================

SET @n      = 100;                    -- ← 要生成几张
SET @name   = '10000点券';             -- 登录器兑换成功时回执里显示的名字
SET @reward = '[{"cera":10000}]';     -- 奖励，JSON 数组（写法见下方注释）
SET @maxuse = 0;                      -- 每张码总兑换上限：0=不限 / 1=一码一人 / N=限量
SET @remark = '批量生成';              -- 备注，给你自己看的

-- 奖励写法示例（可混合多项，顺序即发放顺序）：
--   点券   [{"cera":10000}]
--   物品   [{"item_id":1001,"count":2,"bind":1}]      -- bind 可省略
--   黑钻   [{"black_diamond_days":30}]                -- 1..3650 天，多行按天数累加
--   混合   [{"cera":5000},{"item_id":1001,"count":1,"bind":1},{"black_diamond_days":7}]

DROP PROCEDURE IF EXISTS dnf_gen_cdk;
DELIMITER $$
CREATE PROCEDURE dnf_gen_cdk(
    IN p_count       INT,
    IN p_name        VARCHAR(64),
    IN p_reward_json TEXT,
    IN p_max_redeems INT,
    IN p_remark      VARCHAR(255)
)
BEGIN
    DECLARE chars    VARCHAR(40) DEFAULT 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
    DECLARE code     VARCHAR(32) DEFAULT '';
    DECLARE i        INT DEFAULT 0;
    DECLARE tries    INT DEFAULT 0;
    DECLARE first_id BIGINT DEFAULT 0;
    DECLARE reward   JSON;
    DECLARE rtext    TEXT DEFAULT '';

    IF p_count IS NULL OR p_count < 1 OR p_count > 10000 THEN
        SELECT '数量必须是 1..10000' AS error;
    ELSEIF p_name IS NULL OR TRIM(p_name) = '' THEN
        SELECT '名称不能为空' AS error;
    ELSEIF p_reward_json IS NULL OR JSON_VALID(p_reward_json) = 0
           OR JSON_TYPE(CAST(p_reward_json AS JSON)) <> 'ARRAY' THEN
        SELECT '奖励必须是合法 JSON 数组，例如 [{"cera":10000}]' AS error;
    ELSEIF p_max_redeems IS NULL OR p_max_redeems < 0 THEN
        SELECT '兑换上限必须是 >= 0 的整数（0=不限）' AS error;
    ELSE
        SET reward = CAST(p_reward_json AS JSON);

        -- 奖励段文本：只用于 content_raw 的可读展示，发奖逻辑只认 reward_json
        SELECT COALESCE(GROUP_CONCAT(
                   CASE
                       WHEN jt.item_id > 0 THEN CONCAT(jt.item_id, ':', jt.cnt)
                       WHEN jt.cera    > 0 THEN CONCAT('cera:', jt.cera)
                       WHEN jt.bd      > 0 THEN CONCAT('黑钻:', jt.bd, '天')
                       ELSE ''
                   END ORDER BY jt.ord SEPARATOR ','), '')
          INTO rtext
          FROM JSON_TABLE(reward, '$[*]' COLUMNS (
                   ord     FOR ORDINALITY,
                   item_id INT    PATH '$.item_id',
                   cnt     INT    PATH '$.count',
                   cera    BIGINT PATH '$.cera',
                   bd      BIGINT PATH '$.black_diamond_days'
               )) AS jt;

        -- 记录本次生成的起点，末尾用它把新码列出来
        SELECT COALESCE(MAX(id), 0) INTO first_id FROM redeem_codes;

        WHILE i < p_count AND tries < p_count * 50 DO
            SET tries = tries + 1;

            SET code = CONCAT(
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), '-',
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), '-',
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), '-',
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), '-',
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
                SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1));

            -- 撞码由唯一键 + INSERT IGNORE 兜掉（不做字符串比较，避开
            -- MySQL 8 的 utf8mb4_general_ci / 0900_ai_ci 排序规则冲突）
            INSERT IGNORE INTO redeem_codes
                (redeem_code, name, content_raw, max_redeems, redeemed_count, reward_json, enabled, remark, created_by)
            VALUES
                (code, TRIM(p_name),
                 CONCAT(IF(p_max_redeems = 0, '无限', p_max_redeems), '#', TRIM(p_name), '#', rtext),
                 p_max_redeems, 0, reward, 1, NULLIF(TRIM(p_remark), ''), 'sql-gen');

            IF ROW_COUNT() = 1 THEN
                SET i = i + 1;
            END IF;
        END WHILE;

        SELECT i AS generated_codes, tries AS attempts,
               (SELECT COUNT(*) FROM redeem_codes WHERE enabled = 1) AS enabled_total;

        SELECT id, redeem_code, name, max_redeems, reward_json, remark
          FROM redeem_codes WHERE id > first_id ORDER BY id;
    END IF;
END$$
DELIMITER ;

CALL dnf_gen_cdk(@n, @name, @reward, @maxuse, @remark);

-- 生成的码就是上面第二个结果集里的 redeem_code 列。
-- 想删掉这个存储过程（不影响已生成的码）：
--   DROP PROCEDURE IF EXISTS dnf_gen_cdk;

-- ============================================================
--  之后常用的几条
-- ============================================================
-- 看全部 CDK（含核销进度）：
--   SELECT id, redeem_code, name, max_redeems, redeemed_count, enabled, remark, created_at
--     FROM redeem_codes ORDER BY id DESC;
--
-- 作废一张码：
--   UPDATE redeem_codes SET enabled = 0 WHERE redeem_code = 'XXXX-XXXX-XXXX-XXXX-XXXX';
--
-- 看某张码都被谁兑了：
--   SELECT redeem_code, account_id, username, character_name, mail_status, redeemed_at
--     FROM redeem_redemptions WHERE redeem_code = 'XXXX-XXXX-XXXX-XXXX-XXXX' ORDER BY id;
--
-- 导出还能用的码（在服务器 shell 里执行，不在 mysql 里）：
--   mysql -udnf90 -p"密码" -N dnf_local -e \
--     "SELECT redeem_code FROM redeem_codes WHERE enabled=1 AND (max_redeems=0 OR redeemed_count<max_redeems) ORDER BY id" \
--     > /root/cdk_unused.txt
