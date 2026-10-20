package incidents

import "gorm.io/gorm/clause"

func forUpdate() clause.Expression { return clause.Locking{Strength: "UPDATE"} }
