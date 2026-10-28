-- ============================================================
--  DNF90 注册码批量生成（只改一个数字；已存在的码自动跳过）
--
--  用法：
--    1) 改下面 SET @n 的数字（要生成多少枚）
--    2) 在服务器上执行：
--         mysql -uroot dnf_local < gen_invite_codes.sql
--       或先进入 mysql -uroot dnf_local，再把整段粘进去执行
--
--  说明：
--    · 码格式 ABCD-EFGH，字符集同官方（去掉容易看错的 I O 0 1），一次性
--    · 去重靠主键 + INSERT IGNORE：与表内已有记录（含本次已插入的）不会重复
--    · 只增不改：不动已有的、已用过的记录；可反复执行
--    · 玩家输入时大小写、横线、空格不敏感（服务端比对前会去横线转大写）
-- ============================================================

SET @n = 100;        -- ← 只改这个数字

DROP PROCEDURE IF EXISTS dnf_gen_invites;
DELIMITER $$
CREATE PROCEDURE dnf_gen_invites(IN p_count INT)
BEGIN
  DECLARE i INT DEFAULT 0;
  DECLARE tries INT DEFAULT 0;
  DECLARE code VARCHAR(16) DEFAULT '';
  DECLARE chars VARCHAR(32) DEFAULT 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';

  IF p_count < 1 THEN
    SELECT 'count must be >= 1' AS error;
  ELSE
    WHILE i < p_count AND tries < p_count * 50 DO
      SET tries = tries + 1;
      SET code = CONCAT(
        SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
        SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), '-',
        SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1),
        SUBSTRING(chars, FLOOR(RAND()*32)+1, 1), SUBSTRING(chars, FLOOR(RAND()*32)+1, 1));

      -- 已存在的码由主键 + INSERT IGNORE 自动跳过；不做字符串比较，
      -- 因此不会踩 MySQL 8 的排序规则（utf8mb4_general_ci / 0900_ai_ci）冲突。
      INSERT IGNORE INTO launcher_invites (invite_code, state) VALUES (code, 'unused');
      IF ROW_COUNT() = 1 THEN
        SET i = i + 1;
      END IF;
    END WHILE;

    SELECT i AS generated_codes, tries AS attempts,
           (SELECT COUNT(*) FROM launcher_invites WHERE state = 'unused') AS unused_total;

    SELECT invite_code FROM launcher_invites ORDER BY created_at DESC LIMIT p_count;
  END IF;
END$$
DELIMITER ;

CALL dnf_gen_invites(@n);

-- 用完想删掉这个存储过程（可选，不影响已生成的码）：
-- DROP PROCEDURE IF EXISTS dnf_gen_invites;

-- 导出全部未使用的码（在服务器 shell 里执行，不在 mysql 里）：
-- mysql -uroot -N dnf_local -e "SELECT invite_code FROM launcher_invites WHERE state='unused' ORDER BY created_at DESC" > /root/invites_unused.txt
