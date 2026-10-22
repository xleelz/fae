// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go#L3
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
	chan child_BackupQuotaDropsCapacityRelease30 = [1] of {int}->
	run BackupQuotaDropsCapacityRelease3(child_BackupQuotaDropsCapacityRelease30)->
	child_BackupQuotaDropsCapacityRelease30?0->
stop_process:skip
}

proctype BackupQuotaDropsCapacityRelease3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupQuotaDropsCapacityRelease841 = [1] of {int}->
	chan child_AnonymousBackupQuotaDropsCapacityRelease740 = [1] of {int}->
	Chandef done_ch->
	Chandef capacity_ch->
	

	if
	:: 1 > 0 -> 
		capacity_ch.size = 1->
		run async_monitor(capacity_ch)
	:: else -> 
		run sync_monitor(capacity_ch)
	fi->
	run sync_monitor(done_ch)->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:6:2 */
	:: capacity_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:6:2 */
	:: capacity_ch.sync!false -> 
		capacity_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:6:2 */
	fi->
	run AnonymousBackupQuotaDropsCapacityRelease74(capacity_ch,done_ch,child_AnonymousBackupQuotaDropsCapacityRelease740)->
	run receiver(child_AnonymousBackupQuotaDropsCapacityRelease740)->
	run AnonymousBackupQuotaDropsCapacityRelease84(capacity_ch,done_ch,child_AnonymousBackupQuotaDropsCapacityRelease841)->
	run receiver(child_AnonymousBackupQuotaDropsCapacityRelease841)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:9:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:9:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:9:2 */ -> 
		done_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:10:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:10:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:10:2 */ -> 
		done_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupQuotaDropsCapacityRelease74(Chandef capacity_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:7:12 */
	:: capacity_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:7:12 */->
	:: capacity_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:7:12 */ -> 
		capacity_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:7:23 */
	:: done_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:7:23 */
	:: done_ch.sync!false -> 
		done_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:7:23 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupQuotaDropsCapacityRelease84(Chandef capacity_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:12 */
	:: capacity_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:12 */->
	:: capacity_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:12 */ -> 
		capacity_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:23 */
	:: capacity_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:23 */
	:: capacity_ch.sync!false -> 
		capacity_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:23 */
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:35 */
	:: done_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:35 */
	:: done_ch.sync!false -> 
		done_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/backup_quota.go:8:35 */
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

