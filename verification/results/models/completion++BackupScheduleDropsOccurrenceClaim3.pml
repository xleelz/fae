// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_BackupScheduleDropsOccurrenceClaim30 = [1] of {int}->
	run BackupScheduleDropsOccurrenceClaim3(child_BackupScheduleDropsOccurrenceClaim30)->
	child_BackupScheduleDropsOccurrenceClaim30?0->
stop_process:skip
}

proctype BackupScheduleDropsOccurrenceClaim3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupScheduleDropsOccurrenceClaim1143 = [1] of {int}->
	chan child_AnonymousBackupScheduleDropsOccurrenceClaim1062 = [1] of {int}->
	chan child_AnonymousBackupScheduleDropsOccurrenceClaim941 = [1] of {int}->
	chan child_AnonymousBackupScheduleDropsOccurrenceClaim840 = [1] of {int}->
	Chandef completed_ch->
	Chandef created_ch->
	Chandef claimed_ch->
	Chandef authorized_ch->
	run sync_monitor(authorized_ch)->
	run sync_monitor(claimed_ch)->
	run sync_monitor(created_ch)->
	run sync_monitor(completed_ch)->
	run AnonymousBackupScheduleDropsOccurrenceClaim84(authorized_ch,claimed_ch,created_ch,completed_ch,child_AnonymousBackupScheduleDropsOccurrenceClaim840)->
	run receiver(child_AnonymousBackupScheduleDropsOccurrenceClaim840)->
	run AnonymousBackupScheduleDropsOccurrenceClaim94(authorized_ch,claimed_ch,created_ch,completed_ch,child_AnonymousBackupScheduleDropsOccurrenceClaim941)->
	run receiver(child_AnonymousBackupScheduleDropsOccurrenceClaim941)->
	run AnonymousBackupScheduleDropsOccurrenceClaim106(created_ch,completed_ch,authorized_ch,claimed_ch,child_AnonymousBackupScheduleDropsOccurrenceClaim1062)->
	run receiver(child_AnonymousBackupScheduleDropsOccurrenceClaim1062)->
	run AnonymousBackupScheduleDropsOccurrenceClaim114(authorized_ch,claimed_ch,created_ch,completed_ch,child_AnonymousBackupScheduleDropsOccurrenceClaim1143)->
	run receiver(child_AnonymousBackupScheduleDropsOccurrenceClaim1143)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:12:2 */
	:: completed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:12:2 */->
	:: completed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:12:2 */ -> 
		completed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupScheduleDropsOccurrenceClaim84(Chandef authorized_ch;Chandef claimed_ch;Chandef created_ch;Chandef completed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:8:12 */
	:: authorized_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:8:12 */
	:: authorized_ch.sync!false -> 
		authorized_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:8:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupScheduleDropsOccurrenceClaim94(Chandef authorized_ch;Chandef claimed_ch;Chandef created_ch;Chandef completed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:9:12 */
	:: authorized_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:9:12 */->
	:: authorized_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:9:12 */ -> 
		authorized_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:9:25 */
	:: claimed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:9:25 */
	:: claimed_ch.sync!false -> 
		claimed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:9:25 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupScheduleDropsOccurrenceClaim106(Chandef created_ch;Chandef completed_ch;Chandef authorized_ch;Chandef claimed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:10:12 */
	:: created_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:10:12 */
	:: created_ch.sync!false -> 
		created_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:10:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupScheduleDropsOccurrenceClaim114(Chandef authorized_ch;Chandef claimed_ch;Chandef created_ch;Chandef completed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:11:12 */
	:: created_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:11:12 */->
	:: created_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:11:12 */ -> 
		created_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:11:22 */
	:: completed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:11:22 */
	:: completed_ch.sync!false -> 
		completed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_schedule.go:11:22 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

