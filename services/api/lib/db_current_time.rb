# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

module DbCurrentTime
  CURRENT_TIME_SQL = "SELECT clock_timestamp() AT TIME ZONE 'UTC'"

  def db_current_time
    Time.parse(ActiveRecord::Base.connection.select_value(CURRENT_TIME_SQL) + " +0000")
  end

  def db_transaction_time
    Time.parse(ActiveRecord::Base.connection.select_value("SELECT current_timestamp AT TIME ZONE 'UTC'") + " +0000")
  end
end
